/*
Copyright © The ESO Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package crd

import (
	"encoding/json"
	"time"

	// nolint
	. "github.com/onsi/ginkgo/v2"

	// nolint
	. "github.com/onsi/gomega"
	rbac "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/external-secrets/external-secrets-e2e/framework"
	esv1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esmeta "github.com/external-secrets/external-secrets/apis/meta/v1"
)

// The CRD provider reads arbitrary Kubernetes resources as unstructured objects.
// These e2e tests target a dedicated namespaced test CRD so the scenarios exercise
// the real custom-resource path (discovery, RESTMapper, unstructured read) rather
// than a built-in kind. Remote-cluster connection is intentionally out of scope.
const (
	crdGroup   = "e2e.external-secrets.io"
	crdVersion = "v1alpha1"
	crdKind    = "E2ETestResource"
	crdPlural  = "e2etestresources"
)

var crdGVK = schema.GroupVersionKind{Group: crdGroup, Version: crdVersion, Kind: crdKind}

type Provider struct {
	framework *framework.Framework
}

func NewProvider(f *framework.Framework) *Provider {
	prov := &Provider{
		framework: f,
	}
	BeforeEach(prov.BeforeEach)
	AfterEach(prov.AfterEach)
	return prov
}

func (s *Provider) BeforeEach() {
	s.ensureCRD()
	s.CreateStore()
	s.CreateWhitelistStore()
	s.CreateReferentStore()
}

// AfterEach removes the cluster-scoped objects created for the referent
// ClusterSecretStore. Namespace-scoped objects (Role, RoleBinding, SecretStore,
// the CRs) are garbage-collected with the test namespace by the framework; the
// ClusterRole, ClusterRoleBinding, and ClusterSecretStore are not, so they are
// cleaned up here to avoid leaking across specs. The shared test CRD is left in
// place (the kind cluster is ephemeral).
func (s *Provider) AfterEach() {
	ctx := GinkgoT().Context()
	ns := s.framework.Namespace.Name
	_ = s.framework.CRClient.Delete(ctx, &esv1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: referentStoreName(s.framework)},
	})
	_ = s.framework.CRClient.Delete(ctx, &rbac.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: clusterRoleName(ns)},
	})
	_ = s.framework.CRClient.Delete(ctx, &rbac.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: clusterRoleName(ns)},
	})
}

// CreateSecret seeds an E2ETestResource CR whose spec holds the parsed JSON value.
// The framework calls this for every entry in tc.Secrets.
func (s *Provider) CreateSecret(key string, val framework.SecretEntry) {
	spec := map[string]any{}
	err := json.Unmarshal([]byte(val.Value), &spec)
	Expect(err).ToNot(HaveOccurred())

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crdGVK)
	obj.SetName(key)
	obj.SetNamespace(s.framework.Namespace.Name)
	if len(val.Tags) > 0 {
		obj.SetLabels(val.Tags)
	}
	obj.Object["spec"] = spec

	err = s.framework.CRClient.Create(GinkgoT().Context(), obj)
	Expect(err).ToNot(HaveOccurred())
}

func (s *Provider) DeleteSecret(key string) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crdGVK)
	obj.SetName(key)
	obj.SetNamespace(s.framework.Namespace.Name)
	err := s.framework.CRClient.Delete(GinkgoT().Context(), obj)
	Expect(err).ToNot(HaveOccurred())
}

// ensureCRD installs the shared test CRD if it does not exist and waits until it
// is Established. It is idempotent so parallel specs can call it safely.
func (s *Provider) ensureCRD() {
	ctx := GinkgoT().Context()
	crd := testCRD()
	err := s.framework.CRClient.Create(ctx, crd)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).ToNot(HaveOccurred())
	}
	Eventually(func(g Gomega) {
		got := &apiextensionsv1.CustomResourceDefinition{}
		g.Expect(s.framework.CRClient.Get(ctx, client.ObjectKey{Name: crd.Name}, got)).To(Succeed())
		established := false
		for _, c := range got.Status.Conditions {
			if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
				established = true
			}
		}
		g.Expect(established).To(BeTrue(), "CRD %s is not Established yet", crd.Name)
	}, time.Minute, time.Second).Should(Succeed())
}

func testCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: crdPlural + "." + crdGroup,
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: crdGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   crdPlural,
				Singular: "e2etestresource",
				Kind:     crdKind,
				ListKind: crdKind + "List",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    crdVersion,
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								// spec carries arbitrary content: the provider reads
								// fields out of it via a GJSON path.
								"spec": {
									Type:                   "object",
									XPreserveUnknownFields: new(true),
								},
							},
						},
					},
				},
			},
		},
	}
}

func (s *Provider) storeName() string {
	return s.framework.Namespace.Name
}

func whitelistStoreName(f *framework.Framework) string {
	return f.Namespace.Name + "-wl"
}

func referentStoreName(f *framework.Framework) string {
	return f.Namespace.Name + "-referent"
}

func clusterRoleName(ns string) string {
	return "eso-crd-e2e-" + ns
}

// crdProviderSpec returns a CRD provider that authenticates as the namespace's
// default ServiceAccount (simple in-cluster mode).
func crdProviderSpec() *esv1.CRDProvider {
	return &esv1.CRDProvider{
		ServiceAccountRef: &esmeta.ServiceAccountSelector{Name: "default"},
		Resource: esv1.CRDProviderResource{
			Group:   crdGroup,
			Version: crdVersion,
			Kind:    crdKind,
		},
	}
}

// CreateStore creates the namespaced RBAC granting the default ServiceAccount
// read access to the test CRD, plus the default SecretStore. The same Role and
// RoleBinding also serve the whitelist SecretStore.
func (s *Provider) CreateStore() {
	ns := s.framework.Namespace.Name

	role := &rbac.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "eso-crd-read", Namespace: ns},
		Rules: []rbac.PolicyRule{
			{
				APIGroups: []string{crdGroup},
				Resources: []string{crdPlural},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	}
	rb := &rbac.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "eso-crd-rb", Namespace: ns},
		Subjects: []rbac.Subject{
			{Kind: "ServiceAccount", Name: "default", Namespace: ns},
		},
		RoleRef: rbac.RoleRef{
			Kind:     "Role",
			Name:     role.Name,
			APIGroup: "rbac.authorization.k8s.io",
		},
	}
	Expect(s.framework.CRClient.Create(GinkgoT().Context(), role)).To(Succeed())
	Expect(s.framework.CRClient.Create(GinkgoT().Context(), rb)).To(Succeed())

	store := &esv1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: s.storeName(), Namespace: ns},
		Spec: esv1.SecretStoreSpec{
			Provider: &esv1.SecretStoreProvider{CRD: crdProviderSpec()},
		},
	}
	Expect(s.framework.CRClient.Create(GinkgoT().Context(), store)).To(Succeed())
}

// CreateWhitelistStore creates a second SecretStore with a whitelist that allows
// only names matching "^e2e-crd-.*$" and the property "spec.password". It reuses
// the default-SA Role/RoleBinding created by CreateStore.
func (s *Provider) CreateWhitelistStore() {
	ns := s.framework.Namespace.Name
	prov := crdProviderSpec()
	prov.Whitelist = &esv1.CRDProviderWhitelist{
		Rules: []esv1.CRDProviderWhitelistRule{
			{Name: "^e2e-crd-.*$", Properties: []string{`^spec\.password$`}},
		},
	}
	store := &esv1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: whitelistStoreName(s.framework), Namespace: ns},
		Spec: esv1.SecretStoreSpec{
			Provider: &esv1.SecretStoreProvider{CRD: prov},
		},
	}
	Expect(s.framework.CRClient.Create(GinkgoT().Context(), store)).To(Succeed())
}

// CreateReferentStore creates a referent ClusterSecretStore: its serviceAccountRef
// carries no namespace, so the SA is resolved in the consuming ExternalSecret's
// namespace. For a ClusterSecretStore over a namespaced kind the provider runs a
// cluster-wide SelfSubjectAccessReview, so the SA needs a ClusterRole even when a
// single namespace is read.
func (s *Provider) CreateReferentStore() {
	ns := s.framework.Namespace.Name

	cr := &rbac.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: clusterRoleName(ns)},
		Rules: []rbac.PolicyRule{
			{
				APIGroups: []string{crdGroup},
				Resources: []string{crdPlural},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	}
	crb := &rbac.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: clusterRoleName(ns)},
		Subjects: []rbac.Subject{
			{Kind: "ServiceAccount", Name: "default", Namespace: ns},
		},
		RoleRef: rbac.RoleRef{
			Kind:     "ClusterRole",
			Name:     clusterRoleName(ns),
			APIGroup: "rbac.authorization.k8s.io",
		},
	}
	Expect(s.framework.CRClient.Create(GinkgoT().Context(), cr)).To(Succeed())
	Expect(s.framework.CRClient.Create(GinkgoT().Context(), crb)).To(Succeed())

	css := &esv1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: referentStoreName(s.framework)},
		Spec: esv1.SecretStoreSpec{
			Provider: &esv1.SecretStoreProvider{CRD: crdProviderSpec()},
		},
	}
	Expect(s.framework.CRClient.Create(GinkgoT().Context(), css)).To(Succeed())
}
