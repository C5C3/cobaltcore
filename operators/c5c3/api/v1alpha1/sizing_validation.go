// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"fmt"
	"regexp"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/validation"
)

// sizingStorageSizePattern mirrors the Pattern marker on
// DatabaseSizingSpec.StorageSize.
var sizingStorageSizePattern = regexp.MustCompile(`^[0-9]+(Mi|Gi|Ti)$`)

// minCacheMemoryLimit is the smallest memory limit the Memcached operator
// admits: its maxMemoryMB default (64) plus 32Mi of headroom.
var minCacheMemoryLimit = resource.MustParse("96Mi")

// spreadWhenUnsatisfiable mirrors the Enum marker on
// SpreadConstraintSpec.WhenUnsatisfiable.
var spreadWhenUnsatisfiable = []corev1.UnsatisfiableConstraintAction{corev1.DoNotSchedule, corev1.ScheduleAnyway}

// sizingComponent is one sizable component of a SizingSpec with the field path
// it lives at, so the validators below walk every component uniformly. The
// top level of the SizingSpec is a component too: its placement is the
// fallback of every other.
type sizingComponent struct {
	path              *field.Path
	replicas          *int32
	resources         *corev1.ResourceRequirements
	nodeSelector      map[string]string
	tolerations       []corev1.Toleration
	priorityClassName *string
	spread            []SpreadConstraintSpec
	autoscaling       *commonv1.AutoscalingSpec
}

func containerComponent(path *field.Path, c *ContainerSizingSpec) sizingComponent {
	return sizingComponent{path: path, resources: c.Resources}
}

func pinnedComponent(path *field.Path, p *PinnedSizingSpec) sizingComponent {
	c := containerComponent(path, &p.ContainerSizingSpec)
	c.nodeSelector, c.tolerations, c.priorityClassName = p.NodeSelector, p.Tolerations, p.PriorityClassName
	return c
}

func deploymentComponent(path *field.Path, d *DeploymentSizingSpec) sizingComponent {
	c := pinnedComponent(path, &d.PinnedSizingSpec)
	c.replicas, c.spread = d.Replicas, d.SpreadConstraints
	return c
}

func jobComponent(path *field.Path, j *JobSizingSpec) sizingComponent {
	c := containerComponent(path, &j.ContainerSizingSpec)
	c.priorityClassName = j.PriorityClassName
	return c
}

// sizingComponents returns every component s sets, in a stable order,
// starting with the top level.
func sizingComponents(fldPath *field.Path, s *SizingSpec) []sizingComponent {
	out := []sizingComponent{{
		path:              fldPath,
		nodeSelector:      s.NodeSelector,
		tolerations:       s.Tolerations,
		priorityClassName: s.PriorityClassName,
	}}
	api := func(path *field.Path, a *APISizingSpec) {
		if a != nil {
			c := deploymentComponent(path, &a.DeploymentSizingSpec)
			c.autoscaling = a.Autoscaling
			out = append(out, c)
		}
	}
	deployment := func(path *field.Path, d *DeploymentSizingSpec) {
		if d != nil {
			out = append(out, deploymentComponent(path, d))
		}
	}
	pinned := func(path *field.Path, p *PinnedSizingSpec) {
		if p != nil {
			out = append(out, pinnedComponent(path, p))
		}
	}
	container := func(path *field.Path, c *ContainerSizingSpec) {
		if c != nil {
			out = append(out, containerComponent(path, c))
		}
	}
	jobs := func(path *field.Path, j *JobSizingSpec) {
		if j != nil {
			out = append(out, jobComponent(path, j))
		}
	}

	if d := s.Database; d != nil {
		pinned(fldPath.Child("database"), &d.PinnedSizingSpec)
	}
	if c := s.Cache; c != nil {
		container(fldPath.Child("cache"), &c.ContainerSizingSpec)
	}
	if m := s.Messaging; m != nil {
		pinned(fldPath.Child("messaging"), &m.PinnedSizingSpec)
	}
	container(fldPath.Child("secretStore"), s.SecretStore)
	if ks := s.Keystone; ks != nil {
		p := fldPath.Child("keystone")
		api(p.Child("api"), ks.API)
		jobs(p.Child("jobs"), ks.Jobs)
		container(p.Child("federationProxy"), ks.FederationProxy)
	}
	if hz := s.Horizon; hz != nil && hz.API != nil {
		c := deploymentComponent(fldPath.Child("horizon", "api"), &hz.API.DeploymentSizingSpec)
		c.autoscaling = hz.API.Autoscaling
		out = append(out, c)
	}
	for _, svc := range []struct {
		name string
		spec *APIServiceSizingSpec
	}{{"glance", s.Glance}, {"placement", s.Placement}, {"barbican", s.Barbican}} {
		if svc.spec != nil {
			p := fldPath.Child(svc.name)
			api(p.Child("api"), svc.spec.API)
			jobs(p.Child("jobs"), svc.spec.Jobs)
		}
	}
	if nn := s.Neutron; nn != nil {
		p := fldPath.Child("neutron")
		api(p.Child("api"), nn.API)
		if w := nn.Workers; w != nil {
			pinned(p.Child("workers"), &w.PinnedSizingSpec)
		}
		jobs(p.Child("jobs"), nn.Jobs)
	}
	if cd := s.Cinder; cd != nil {
		p := fldPath.Child("cinder")
		api(p.Child("api"), cd.API)
		deployment(p.Child("scheduler"), cd.Scheduler)
		pinned(p.Child("volume"), cd.Volume)
		pinned(p.Child("backup"), cd.Backup)
		jobs(p.Child("jobs"), cd.Jobs)
	}
	if nv := s.Nova; nv != nil {
		p := fldPath.Child("nova")
		api(p.Child("api"), nv.API)
		if m := nv.Metadata; m != nil {
			deployment(p.Child("metadata"), &m.DeploymentSizingSpec)
		}
		if w := nv.Scheduler; w != nil {
			deployment(p.Child("scheduler"), &w.DeploymentSizingSpec)
		}
		if w := nv.Conductor; w != nil {
			deployment(p.Child("conductor"), &w.DeploymentSizingSpec)
		}
		deployment(p.Child("consoleProxy"), nv.ConsoleProxy)
		jobs(p.Child("jobs"), nv.Jobs)
	}
	return out
}

// validateSizingSpec checks the values of one SizingSpec as written, for the
// ControlPlane's spec.sizing and a SizingProfile's spec alike. It runs the
// checks the schema cannot express (requests within limits, the label grammar
// of node selectors and tolerations, the Galera quorum and the Memcached
// memory floor) and the webhook twins of the markers on the spread entries and
// the database volume size.
func validateSizingSpec(fldPath *field.Path, s *SizingSpec) field.ErrorList {
	var allErrs field.ErrorList
	for _, c := range sizingComponents(fldPath, s) {
		allErrs = append(allErrs, validation.RequestsWithinLimits(c.path.Child("resources"), c.resources)...)
		allErrs = append(allErrs, validation.NodeSelectorLabels(c.path.Child("nodeSelector"), c.nodeSelector)...)
		allErrs = append(allErrs, validation.Tolerations(c.path.Child("tolerations"), c.tolerations)...)
		for i, sc := range c.spread {
			scPath := c.path.Child("spreadConstraints").Index(i)
			if sc.MaxSkew < 1 {
				allErrs = append(allErrs, field.Invalid(scPath.Child("maxSkew"), sc.MaxSkew, "must be at least 1"))
			}
			if sc.TopologyKey == "" {
				allErrs = append(allErrs, field.Required(scPath.Child("topologyKey"), "must be set"))
			}
			if !slices.Contains(spreadWhenUnsatisfiable, sc.WhenUnsatisfiable) {
				allErrs = append(allErrs, field.NotSupported(scPath.Child("whenUnsatisfiable"),
					sc.WhenUnsatisfiable, spreadWhenUnsatisfiable))
			}
		}
	}
	if db := s.Database; db != nil {
		dbPath := fldPath.Child("database")
		if db.Replicas != nil {
			allErrs = append(allErrs, validateDatabaseReplicas(dbPath, &commonv1.DatabaseSpec{Replicas: *db.Replicas})...)
		}
		if db.StorageSize != "" && !sizingStorageSizePattern.MatchString(db.StorageSize) {
			allErrs = append(allErrs, field.Invalid(dbPath.Child("storageSize"), db.StorageSize,
				fmt.Sprintf("must match %s (e.g. 512Mi, 100Gi)", sizingStorageSizePattern)))
		}
	}
	if c := s.Cache; c != nil && c.Resources != nil {
		if limit, ok := c.Resources.Limits[corev1.ResourceMemory]; ok && limit.Cmp(minCacheMemoryLimit) < 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("cache", "resources", "limits", "memory"),
				limit.String(),
				fmt.Sprintf("memory limit must be at least %s: the Memcached operator requires maxMemoryMB (64) plus 32Mi",
					minCacheMemoryLimit.String())))
		}
	}
	return allErrs
}

// validateResolvedSizing checks a merged SizingSpec, the one a ControlPlane
// actually projects: a request a profile sets may exceed a limit the
// ControlPlane sets, a request of zero may meet an autoscaling target set
// elsewhere, or a replica count may exceed a maxReplicas set elsewhere. It
// checks requests within limits on every component, and every API autoscaling
// block against its component's requests and replica count. The Keystone
// federation proxy runs in the API pods, so its resources meet the Keystone
// target too.
//
// The replica check is the child webhooks' rule: without minReplicas the HPA
// minimum defaults to the projected replica count (commonv1.DefaultReplicas
// when the component names none), so a maxReplicas below it would have every
// child reject the projection.
func validateResolvedSizing(fldPath *field.Path, s *SizingSpec) field.ErrorList {
	var allErrs field.ErrorList
	for _, c := range sizingComponents(fldPath, s) {
		rrPath := c.path.Child("resources")
		allErrs = append(allErrs, validation.RequestsWithinLimits(rrPath, c.resources)...)
		allErrs = append(allErrs, validation.AutoscalingTargetRequests(rrPath, c.resources, c.autoscaling)...)
		if a := c.autoscaling; a != nil && a.MinReplicas == nil {
			if replicas := ptr.Deref(c.replicas, commonv1.DefaultReplicas); replicas > a.MaxReplicas {
				allErrs = append(allErrs, field.Invalid(c.path.Child("autoscaling", "maxReplicas"), a.MaxReplicas,
					fmt.Sprintf("maxReplicas must be >= replicas (%d) when minReplicas is not set, "+
						"because minReplicas defaults to replicas", replicas)))
			}
		}
	}
	if ks := s.Keystone; ks != nil && ks.API != nil && ks.FederationProxy != nil {
		allErrs = append(allErrs, validation.AutoscalingTargetRequests(
			fldPath.Child("keystone", "federationProxy", "resources"), ks.FederationProxy.Resources, ks.API.Autoscaling)...)
	}
	return allErrs
}

// sizingPriorityClass is one priority class name a SizingSpec sets, with the
// path of the first component that names it.
type sizingPriorityClass struct {
	path *field.Path
	name string
}

// sizingPriorityClassNames returns every distinct non-empty priority class
// name s sets, in the stable order of sizingComponents. An empty name opts a
// component out of the top-level class and names no PriorityClass.
func sizingPriorityClassNames(fldPath *field.Path, s *SizingSpec) []sizingPriorityClass {
	var out []sizingPriorityClass
	seen := map[string]struct{}{}
	for _, c := range sizingComponents(fldPath, s) {
		if c.priorityClassName == nil || *c.priorityClassName == "" {
			continue
		}
		name := *c.priorityClassName
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, sizingPriorityClass{path: c.path.Child("priorityClassName"), name: name})
	}
	return out
}

// validateNewPriorityClasses looks up every PriorityClass newSpec names that
// oldSpec did not; oldSpec is nil on create. A class deleted after the object
// was admitted therefore does not block an unrelated edit.
func validateNewPriorityClasses(
	ctx context.Context, r client.Reader, fldPath *field.Path, oldSpec, newSpec *SizingSpec,
) field.ErrorList {
	kept := map[string]struct{}{}
	if oldSpec != nil {
		for _, pc := range sizingPriorityClassNames(fldPath, oldSpec) {
			kept[pc.name] = struct{}{}
		}
	}
	var allErrs field.ErrorList
	for _, pc := range sizingPriorityClassNames(fldPath, newSpec) {
		if _, ok := kept[pc.name]; !ok {
			allErrs = append(allErrs, validation.PriorityClassExists(ctx, r, pc.path, pc.name)...)
		}
	}
	return allErrs
}

// sizingInertWarnings returns one warning per spec.sizing.<service> block
// whose service the ControlPlane does not declare: nothing projects the values.
func sizingInertWarnings(cp *ControlPlane) admission.Warnings {
	s := cp.Spec.Sizing
	if s == nil {
		return nil
	}
	svc := cp.Spec.Services
	var warnings admission.Warnings
	for _, b := range []struct {
		name           string
		sized, running bool
	}{
		{"keystone", s.Keystone != nil, svc.Keystone != nil},
		{"horizon", s.Horizon != nil, svc.Horizon != nil},
		{"glance", s.Glance != nil, svc.Glance != nil},
		{"placement", s.Placement != nil, svc.Placement != nil},
		{"barbican", s.Barbican != nil, svc.Barbican != nil},
		{"neutron", s.Neutron != nil, svc.Neutron != nil},
		{"cinder", s.Cinder != nil, svc.Cinder != nil},
		{"nova", s.Nova != nil, svc.Nova != nil},
	} {
		if b.sized && !b.running {
			warnings = append(warnings, fmt.Sprintf(
				"spec.sizing.%s is set but spec.services.%s is not; the values are inert", b.name, b.name))
		}
	}
	return warnings
}
