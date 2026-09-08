// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package satellite

import (
	"context"
	"errors"
	"testing"

	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	testVolume  = "config"
	testDataKey = "backends.conf"
)

var testDeployKey = client.ObjectKey{Namespace: "ns", Name: "parent"}

func TestSecretNameForVolume(t *testing.T) {
	cases := []struct {
		name string
		spec *corev1.PodSpec
		want string
	}{
		{name: "nil pod spec", spec: nil},
		{name: "no volumes at all", spec: &corev1.PodSpec{}},
		{
			name: "another volume carries a Secret",
			spec: &corev1.PodSpec{Volumes: []corev1.Volume{{
				Name:         "other",
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "other-secret"}},
			}}},
		},
		{
			name: "the named volume is ConfigMap-backed",
			spec: &corev1.PodSpec{Volumes: []corev1.Volume{
				{Name: testVolume, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
				{Name: "other", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "other-secret"}}},
			}},
		},
		{
			name: "the named volume is Secret-backed",
			spec: &corev1.PodSpec{Volumes: []corev1.Volume{
				{Name: "other", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
				{Name: testVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "rendered"}}},
			}},
			want: "rendered",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(SecretNameForVolume(tc.spec, testVolume)).To(gomega.Equal(tc.want))
		})
	}
}

func TestSectionPresent(t *testing.T) {
	cases := []struct {
		name    string
		conf    string
		section string
		want    bool
	}{
		{name: "exact match on its own line", conf: "[store]\nk = v\n", section: "store", want: true},
		{name: "match at start of data", conf: "[store]", section: "store", want: true},
		{name: "match at end without trailing newline", conf: "[a]\nk = v\n[store]", section: "store", want: true},
		{name: "match with CRLF line ending", conf: "[store]\r\nk = v\r\n", section: "store", want: true},
		{name: "prefix collision is not a match", conf: "[store2]\nk = v\n", section: "store", want: false},
		{name: "suffix collision is not a match", conf: "[xstore]\nk = v\n", section: "store", want: false},
		{name: "header inside a value line is not a match", conf: "note = see [store]\n", section: "store", want: false},
		{name: "absent section", conf: "[other]\nk = v\n", section: "store", want: false},
		{name: "empty data", conf: "", section: "store", want: false},

		// A one-character name leaves the least room between the token and its
		// neighbours, so it is the sharpest test of the boundary check.
		{name: "short name at start of data", conf: "[a]\nk = v\n", section: "a", want: true},
		{name: "short name in the middle of data", conf: "[b]\nk = v\n[a]\nk = v\n", section: "a", want: true},
		{name: "short name at end of data", conf: "[b]\nk = v\n[a]", section: "a", want: true},
		{name: "short name before a bare CR", conf: "[a]\rk = v\r", section: "a", want: true},
		{name: "short name with a suffix is not a match", conf: "[a]b\n", section: "a", want: false},
		{name: "short name with a prefix is not a match", conf: "x[a]\n", section: "a", want: false},
		{name: "short name inside a value line is not a match", conf: "key = [a]\n", section: "a", want: false},
		{name: "a rejected occurrence does not hide a later match", conf: "key = [a]\n[a]\n", section: "a", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(SectionPresent([]byte(tc.conf), "["+tc.section+"]")).To(gomega.Equal(tc.want))
		})
	}
}

// testDeploy builds the parent's Deployment with the given volumes.
func testDeploy(volumes ...corev1.Volume) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: testDeployKey.Namespace, Name: testDeployKey.Name},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: volumes}},
		},
	}
}

func secretVolume(secretName string) corev1.Volume {
	return corev1.Volume{
		Name:         testVolume,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName}},
	}
}

func testObserveParams(children client.Client) ObserveParams {
	return ObserveParams{
		Children:      children,
		DeploymentKey: testDeployKey,
		VolumeName:    testVolume,
		DataKey:       testDataKey,
		SectionHeader: "[store]",
	}
}

func TestSectionProjected(t *testing.T) {
	rendered := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testDeployKey.Namespace, Name: "rendered"},
		Data:       map[string][]byte{testDataKey: []byte("[store]\nk = v\n")},
	}

	cases := []struct {
		name    string
		objects []client.Object
		want    bool
	}{
		{name: "the Deployment does not exist yet", objects: nil},
		{name: "the Deployment carries no such volume", objects: []client.Object{testDeploy()}},
		{
			name: "the volume is not Secret-backed",
			objects: []client.Object{testDeploy(corev1.Volume{
				Name:         testVolume,
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}},
			})},
		},
		{name: "the projection Secret does not exist yet", objects: []client.Object{testDeploy(secretVolume("rendered"))}},
		{
			name: "the projection Secret carries no such data key",
			objects: []client.Object{
				testDeploy(secretVolume("rendered")),
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Namespace: testDeployKey.Namespace, Name: "rendered"},
					Data:       map[string][]byte{"other.conf": []byte("[store]\n")},
				},
			},
		},
		{
			name:    "the section is projected",
			objects: []client.Object{testDeploy(secretVolume("rendered")), rendered},
			want:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			children := fake.NewClientBuilder().
				WithScheme(clientgoscheme.Scheme).
				WithObjects(tc.objects...).
				Build()

			got, err := SectionProjected(context.Background(), testObserveParams(children))

			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(got).To(gomega.Equal(tc.want))
		})
	}

	t.Run("a failing Deployment read is wrapped", func(t *testing.T) {
		g := gomega.NewWithT(t)
		children := fake.NewClientBuilder().
			WithScheme(clientgoscheme.Scheme).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*appsv1.Deployment); ok {
						return errors.New("boom")
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}).
			Build()

		got, err := SectionProjected(context.Background(), testObserveParams(children))

		g.Expect(got).To(gomega.BeFalse())
		g.Expect(err).To(gomega.MatchError("fetching Deployment ns/parent: boom"))
	})

	t.Run("a failing projection Secret read is wrapped", func(t *testing.T) {
		g := gomega.NewWithT(t)
		children := fake.NewClientBuilder().
			WithScheme(clientgoscheme.Scheme).
			WithObjects(testDeploy(secretVolume("rendered"))).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.Secret); ok {
						return errors.New("boom")
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}).
			Build()

		got, err := SectionProjected(context.Background(), testObserveParams(children))

		g.Expect(got).To(gomega.BeFalse())
		g.Expect(err).To(gomega.MatchError("fetching projection Secret ns/rendered: boom"))
	})
}
