package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
)

// In true go fashion, I spent most of my time trying to resolve go mod issues
// so that I could import this one thing, I am now including it here directly
// for the time being. This is sourced from ArgoCD codebase located here:
//
//	https://github.com/argoproj/argo-cd/blob/master/pkg/apis/application/v1alpha1/types.go
type ClusterConfig struct {
	// Server requires Bearer authentication. This client will not attempt to use
	// refresh tokens for an OAuth2 flow.
	// TODO: demonstrate an OAuth2 compatible client.
	BearerToken string `json:"bearerToken,omitempty" protobuf:"bytes,3,opt,name=bearerToken"`
	// TLSClientConfig contains settings to enable transport layer security
	TLSClientConfig `json:"tlsClientConfig" protobuf:"bytes,4,opt,name=tlsClientConfig"`
}

// TLSClientConfig contains settings to enable transport layer security
type TLSClientConfig struct {
	// Insecure specifies that the server should be accessed without verifying the TLS certificate. For testing only.
	Insecure bool `json:"insecure" protobuf:"bytes,1,opt,name=insecure"`
	// CAData holds PEM-encoded bytes (typically read from a root certificates bundle).
	// CAData takes precedence over CAFile
	CAData []byte `json:"caData,omitempty" protobuf:"bytes,5,opt,name=caData"`
}

// GetMgmtClusterConfig returns a kubernetes client config for the management cluster
func GetMgmtClusterConfig() (*kubernetes.Clientset, error) {
	conf, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(conf)
}

// GetTargetClusterConfig returns a kubernetes client config for the target cluster.
// This uses the structure of cluster-api secrets to retrieve the kubeconfig for the
// target cluster. The secret must be named <cluster-name>-kubeconfig and must contain
// a key named "value" with the kubeconfig for the target cluster.
func GetTargetClusterConfig(mgmtClientset *kubernetes.Clientset, namespace string, name string) (*kubernetes.Clientset, *rest.Config, error) {
	secret, err := mgmtClientset.CoreV1().Secrets(namespace).Get(context.Background(), fmt.Sprintf("%s-kubeconfig", name), v1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	targetConf, err := clientcmd.RESTConfigFromKubeConfig(secret.Data["value"])
	if err != nil {
		return nil, nil, err
	}
	clientset, err := kubernetes.NewForConfig(targetConf)

	return clientset, targetConf, err
}

// CreateServiceAccount creates a service account for argocd role in target cluster
// we are not currently trying to update this service account if it exists because
// name and namespace are immutable and we aren't setting any other properties.
func CreateServiceAccount(ctx context.Context, clientset *kubernetes.Clientset, name string) (*corev1.ServiceAccount, error) {
	serviceAccount, err := clientset.CoreV1().ServiceAccounts("kube-system").Get(ctx, name, v1.GetOptions{})
	if err != nil {
		// if not found create the resource
		if errors.IsNotFound(err) {
			return clientset.CoreV1().ServiceAccounts("kube-system").Create(ctx, &corev1.ServiceAccount{
				ObjectMeta: v1.ObjectMeta{
					Name:      name,
					Namespace: "kube-system",
				},
			}, v1.CreateOptions{})
		}
		// if other error return it
		return nil, err
	}
	return serviceAccount, nil
}

func CreateServiceAccountSecret(ctx context.Context, clientset *kubernetes.Clientset, name string) (*corev1.Secret, error) {
	serviceAccountSecret, err := clientset.CoreV1().Secrets("kube-system").Get(ctx, name, v1.GetOptions{})
	if err != nil {
		// if not found create the resource
		if errors.IsNotFound(err) {
			return clientset.CoreV1().Secrets("kube-system").Create(ctx, &corev1.Secret{
				ObjectMeta: v1.ObjectMeta{
					Name:      name,
					Namespace: "kube-system",
					Annotations: map[string]string{
						"kubernetes.io/service-account.name": name,
					},
				},
				Type: corev1.SecretTypeServiceAccountToken,
			}, v1.CreateOptions{})
		}
		// if other error return it
		return nil, err
	}
	return serviceAccountSecret, nil
}

// CreateOrUpdateClusterRole creates a cluster role in the target cluster
// for argocd.
func CreateOrUpdateClusterRole(ctx context.Context, clientset *kubernetes.Clientset) (*rbacv1.ClusterRole, error) {
	desired := &rbacv1.ClusterRole{
		ObjectMeta: v1.ObjectMeta{
			Name:      "argocd-manager-role",
			Namespace: "kube-system",
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"*"},
				Resources: []string{"*"},
				Verbs:     []string{"*"},
			},
			{
				NonResourceURLs: []string{"*"},
				Verbs:           []string{"*"},
			},
		},
	}

	_, err := clientset.RbacV1().ClusterRoles().Get(ctx, "argocd-manager-role", v1.GetOptions{})
	if err != nil {
		// if not found, create the cluster role
		if errors.IsNotFound(err) {
			return clientset.RbacV1().ClusterRoles().Create(ctx, desired, v1.CreateOptions{})
		}
		// if other error return it
		return nil, err
	}

	// if found, update the cluster role
	return clientset.RbacV1().ClusterRoles().Update(ctx, desired, v1.UpdateOptions{})
}

// CreateOrUpdateClusterRoleBinding creates a cluster role binding in the target cluster
// for argocd.
func CreateOrUpdateClusterRoleBinding(ctx context.Context, clientset *kubernetes.Clientset) (*rbacv1.ClusterRoleBinding, error) {
	desired := &rbacv1.ClusterRoleBinding{
		ObjectMeta: v1.ObjectMeta{
			Name:      "argocd-manager-role-binding",
			Namespace: "kube-system",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "argocd-manager-role",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "argocd-manager",
				Namespace: "kube-system",
			},
		},
	}

	_, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, "argocd-manager-role-binding", v1.GetOptions{})
	if err != nil {
		// if not found, create the cluster role
		if errors.IsNotFound(err) {
			return clientset.RbacV1().ClusterRoleBindings().Create(ctx, desired, v1.CreateOptions{})
		}
		// if other error return it
		return nil, err
	}

	// if found, update the cluster role
	return clientset.RbacV1().ClusterRoleBindings().Update(ctx, desired, v1.UpdateOptions{})
}

// GetServiceAccountBearerToken returns a bearer token for the service account
// this token is used to authenticate with the target cluster along with the CA
func GetServiceAccountBearerToken(ctx context.Context, clientset *kubernetes.Clientset, svcscrt corev1.Secret) ([]byte, error) {
	return svcscrt.Data["token"], nil
}
