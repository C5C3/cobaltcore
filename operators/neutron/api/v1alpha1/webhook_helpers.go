// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/c5c3/cobaltcore/internal/common/config"
	"github.com/c5c3/cobaltcore/internal/common/release"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/internal/common/validation"
)

// validateImage checks a required image reference. It repeats, in the webhook
// layer, what the +kubebuilder:validation markers and the XValidation rule on
// commonv1.ImageSpec already enforce, so the invariant still holds where a
// schema-layer check is bypassed.
func validateImage(fldPath *field.Path, image commonv1.ImageSpec) field.ErrorList {
	var errs field.ErrorList
	if image.Repository == "" {
		errs = append(errs, field.Required(fldPath.Child("repository"), "repository must be set"))
	}
	if (image.Tag != "") == (image.Digest != "") {
		errs = append(errs, field.Invalid(
			fldPath, image, "exactly one of image.tag or image.digest must be set",
		))
	}
	return errs
}

// validateExtraConfigShape rejects the spec.extraConfig shapes that reach the
// rendered configuration file as something other than one option: an empty
// section name or an empty option key so the file never carries a nameless
// [<section>] or a bare "= value" line, a newline or carriage return anywhere in
// a section name, a key or a value, and an override of a Rejected owned key.
//
// extraConfig is a preserve-unknown-fields map, so CEL cannot constrain its
// keys: the webhook is the sole admission-time gate for all of it. The
// control-character rule matters because the INI renderer writes each part
// verbatim ("[%s]" for the section, "%s = %s" per option), so such a character
// injects arbitrary additional config lines, smuggling a whole [section]/key past
// the (section, key)-name-keyed ownership (FindOwnedOverrides) and catalog
// (FindUnknownOptions) gates, which inspect map structure only and never look
// inside a value.
//
// The Rejected owned keys are the ones honoring the override would already have
// damaged by the time the ExtraConfigHealthy condition could surface it: each is
// either a credential the rendering would copy into the config Secret every pod
// mounts, or a path or connection string that points a process somewhere the
// operator did not provision. registry selects the kind's own list.
func validateExtraConfigShape(specPath *field.Path, extraConfig map[string]map[string]string, registry []config.OwnedKey) field.ErrorList {
	var errs field.ErrorList
	extraConfigPath := specPath.Child("extraConfig")

	for section, opts := range extraConfig {
		if section == "" {
			errs = append(errs, field.Invalid(
				extraConfigPath,
				section,
				"extraConfig section name must not be empty",
			))
			continue
		}
		if validation.HasControlChars(section) {
			errs = append(errs, field.Invalid(
				extraConfigPath,
				section,
				"extraConfig section name must not contain a newline or carriage return",
			))
			continue
		}
		for key, value := range opts {
			if key == "" {
				errs = append(errs, field.Invalid(
					extraConfigPath.Key(section),
					key,
					"extraConfig key must not be empty",
				))
				continue
			}
			if validation.HasControlChars(key) || validation.HasControlChars(value) {
				errs = append(errs, field.Invalid(
					extraConfigPath.Key(section).Key(key),
					key,
					"extraConfig key and value must not contain a newline or carriage return: "+
						"the rendered INI writes them verbatim, so a newline injects arbitrary config lines",
				))
			}
		}
	}

	for _, e := range registry {
		if !e.Rejected {
			continue
		}
		if _, ok := extraConfig[e.Section][e.Key]; ok {
			msg := fmt.Sprintf("%s is managed via %s and must not be set in extraConfig", e.Key, e.OwnedBy)
			if e.Impact != "" {
				msg += fmt.Sprintf(" (%s)", e.Impact)
			}
			errs = append(errs, field.Forbidden(
				extraConfigPath.Key(e.Section).Key(e.Key),
				msg,
			))
		}
	}

	return errs
}

// validateExtraConfigOptions validates the option names in extraConfig against
// the option catalog embedded for the release named by openStackRelease. Both
// kinds share it: the catalog is the flat union of neutron.conf, ml2_conf.ini and
// neutron_ovn_metadata_agent.ini, so the same catalog judges both files.
//
// It fails open: when openStackRelease cannot be resolved to an embedded catalog
// it returns exactly one warning and no errors, so a value that does not name a
// release or a build that ships no catalog for the release never blocks
// admission. registry carries the kind's owned keys, which are exempt from the
// scan so an operator-owned key never doubles as an unknown option. Every
// remaining unknown option or unknown section becomes a field error; every
// deprecated-but-accepted option becomes a warning naming its replacement.
func validateExtraConfigOptions(
	specPath *field.Path,
	openStackRelease string,
	extraConfig map[string]map[string]string,
	registry []config.OwnedKey,
) (admission.Warnings, field.ErrorList) {
	if len(extraConfig) == 0 {
		return nil, nil
	}

	catalog, ok := OptionCatalogForRelease(openStackRelease)
	if !ok {
		// Fail open with exactly one warning. Distinguish a value that does not name
		// a release at all from a parseable release the operator ships no catalog
		// for.
		rel, err := release.ParseRelease(openStackRelease)
		if err != nil {
			return admission.Warnings{
				"spec.extraConfig was not validated against an option catalog: " +
					"spec.openStackRelease does not name an OpenStack release",
			}, nil
		}
		return admission.Warnings{
			fmt.Sprintf("spec.extraConfig was not validated against an option catalog: "+
				"no catalog for release %q is embedded in this operator build",
				fmt.Sprintf("%d.%d", rel.Year, rel.Minor)),
		}, nil
	}

	base := catalog.Release
	unknown, deprecated := catalog.FindUnknownOptions(extraConfig, config.CatalogExemptions{
		Keys: config.KeyExemptionsFromRegistry(registry),
	})

	extraConfigPath := specPath.Child("extraConfig")
	var errs field.ErrorList
	for _, u := range unknown {
		fldPath := extraConfigPath.Key(u.Section).Key(u.Key)
		if u.SectionUnknown {
			errs = append(errs, field.Invalid(fldPath, u.Key,
				fmt.Sprintf("no such section in the neutron %s option catalog", base)))
			continue
		}
		errs = append(errs, field.Invalid(fldPath, u.Key,
			fmt.Sprintf("no such option in the neutron %s option catalog", base)))
	}

	var warnings admission.Warnings
	for _, d := range deprecated {
		if d.Replacement == "" {
			warnings = append(warnings, fmt.Sprintf(
				"spec.extraConfig [%s] %s: deprecated option in neutron %s with no replacement",
				d.Section, d.Key, base,
			))
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"spec.extraConfig [%s] %s: deprecated option in neutron %s, replaced by %s",
			d.Section, d.Key, base, d.Replacement,
		))
	}
	return warnings, errs
}

// extraConfigCatalogInputsChanged reports whether anything the extraConfig
// option-catalog check depends on differs between the old and the new revision:
// the extraConfig map itself, or the openStackRelease that selects the release
// catalog. Both kinds' ValidateUpdate gates the catalog re-validation on this so
// a CR whose extraConfig went stale-invalid is not rejected by an
// otherwise-unrelated update.
func extraConfigCatalogInputsChanged(
	oldRelease, newRelease string,
	oldExtraConfig, newExtraConfig map[string]map[string]string,
) bool {
	if oldRelease != newRelease {
		return true
	}
	return !reflect.DeepEqual(oldExtraConfig, newExtraConfig)
}

// validLogLevels is the oslo.log level set both kinds accept, for the root
// logger and for every named logger in spec.logging.perLoggerLevels.
var validLogLevels = map[string]struct{}{
	"DEBUG":    {},
	"INFO":     {},
	"WARNING":  {},
	"ERROR":    {},
	"CRITICAL": {},
}

// validateLogging is the defense-in-depth twin of the CRD enum markers on
// LoggingSpec.Format / .Level, plus the per-logger checks that have no schema
// counterpart: map values cannot be expressed as a CRD enum on
// additionalProperties, and a logger name renders into the [DEFAULT]
// default_log_levels CSV, which the INI renderer writes verbatim, so a newline in
// it injects arbitrary config lines the same way an extraConfig key would.
//
// configFile names the file the values are rendered into, so the message points
// at the file the kind under validation actually writes. A nil block carries
// nothing to validate.
func validateLogging(fldPath *field.Path, logging *commonv1.LoggingSpec, configFile string) field.ErrorList {
	if logging == nil {
		return nil
	}
	var errs field.ErrorList

	if logging.Format != "" && logging.Format != "text" && logging.Format != "json" {
		errs = append(errs, field.NotSupported(
			fldPath.Child("format"), logging.Format, []string{"text", "json"},
		))
	}
	if logging.Level != "" {
		if _, ok := validLogLevels[logging.Level]; !ok {
			errs = append(errs, field.NotSupported(
				fldPath.Child("level"), logging.Level,
				[]string{"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"},
			))
		}
	}

	perLoggerPath := fldPath.Child("perLoggerLevels")
	for name, lvl := range logging.PerLoggerLevels {
		if name == "" {
			errs = append(errs, field.Invalid(
				perLoggerPath, name, "logger name must not be empty",
			))
			continue
		}
		if validation.HasControlChars(name) {
			errs = append(errs, field.Invalid(
				perLoggerPath, name,
				"logger name must not contain a newline or carriage return: it is rendered "+
					"verbatim into "+configFile+", so a newline injects arbitrary config lines",
			))
			continue
		}
		if _, ok := validLogLevels[lvl]; !ok {
			errs = append(errs, field.Invalid(
				perLoggerPath.Key(name), lvl,
				"level must be one of DEBUG, INFO, WARNING, ERROR, CRITICAL",
			))
		}
	}
	return errs
}
