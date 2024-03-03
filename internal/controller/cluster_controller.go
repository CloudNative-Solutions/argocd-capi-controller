/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	capi "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ClusterReconciler reconciles a Cluster object
type ClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters/status,verbs=get
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.16.3/pkg/reconcile
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	log := log.Log.WithValues("cluster", req.NamespacedName)

	// retrieve the cluster object
	var cluster capi.Cluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		log.Error(err, "unable to fetch cluster")
		return ctrl.Result{}, err
	}

	// if control plane is not ready, return and requeue
	if !cluster.Status.ControlPlaneReady {
		log.Info(fmt.Sprintf("cluster %s is not ready", cluster.Name))
		return ctrl.Result{Requeue: true}, nil
	}

	// check status of cluster controlplane
	// if controlplane is ready let's do some stuff
	log.Info(fmt.Sprintf("cluster %s is ready", cluster.Name))

	// create connection to management cluster
	clientset, err := GetMgmtClusterConfig()
	if err != nil {
		log.Error(err, "unable to create client for management cluster")
		return ctrl.Result{}, err
	}

	// create connection to target cluster
	targetClientset, targetConf, err := GetTargetClusterConfig(clientset, cluster.Namespace, cluster.Name)
	if err != nil {
		log.Error(err, "unable to create client config for target cluster")
		return ctrl.Result{}, err
	}

	// create serviceaccount in target cluster
	svc, err := CreateServiceAccount(ctx, targetClientset, "argocd-manager")
	if err != nil {
		log.Error(err, "unable to create serviceaccount")
		return ctrl.Result{Requeue: true}, err
	}

	// create serviceaccount secret in target cluster
	svcscrt, err := CreateServiceAccountSecret(ctx, targetClientset, svc.Name)
	if err != nil {
		log.Error(err, "unable to create serviceaccount secret")
		return ctrl.Result{Requeue: true}, err
	}

	// retrieve the serviceaccount bearer token from target cluster
	token, err := GetServiceAccountBearerToken(ctx, targetClientset, *svcscrt)
	if err != nil {
		log.Error(err, "unable to get serviceaccount bearer token")
		return ctrl.Result{Requeue: true}, err
	}

	// create clusterrolebinding in target cluster
	if _, err := CreateOrUpdateClusterRole(ctx, targetClientset); err != nil {
		log.Error(err, "unable to create or update clusterrole")
		return ctrl.Result{Requeue: true}, err
	}

	// create clusterrolebinding in target cluster
	if _, err := CreateOrUpdateClusterRoleBinding(ctx, targetClientset); err != nil {
		log.Error(err, "unable to create or update clusterrolebinding")
		return ctrl.Result{Requeue: true}, err
	}

	// TODO: create argocd cluster secret structure
	// we need a `config` key that follows this structure:
	// 		https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/#clusters
	// for our other clusters it seems we just have tlsClientConfig.caData set and insecure set

	// create config structure for argocd
	clusterConfig := ClusterConfig{
		BearerToken: string(token),
		TLSClientConfig: TLSClientConfig{
			Insecure: false,
			CAData:   targetConf.TLSClientConfig.CAData,
		},
	}
	// marshall config to json
	data, err := json.Marshal(clusterConfig)
	if err != nil {
		log.Error(err, "unable to marshall cluster config")
		return ctrl.Result{}, err
	}

	// pass cluster labels/annotations to secret
	// this provides metadata for our applicationsets
	// in argocd
	labels := make(map[string]string)
	// add argocd type label so cluster can be found
	labels["argocd.argoproj.io/secret-type"] = "cluster"
	// add labels from cluster if any exist
	for k, v := range cluster.GetLabels() {
		labels[k] = v
	}
	// remove argocd.argoproj.io/instance to not show in ArgoCD cluster application.
	delete(labels, "argocd.argoproj.io/instance")

	// desired secret for argocd cluster
	clusterSecret := &corev1.Secret{
		ObjectMeta: v1.ObjectMeta{
			Name:      fmt.Sprintf("cluster-%s", cluster.Name),
			Namespace: "argocd",
			Labels:    labels,
		},
		Type: "Opaque",
		Data: map[string][]byte{
			"config": data,
			"name":   []byte(cluster.Name),
			"server": []byte(targetConf.Host),
		},
	}

	// create cluster secret
	log.Info("cluster", "servername", cluster.Name, "host", targetConf.Host)
	_, err = clientset.CoreV1().Secrets("argocd").Get(ctx, fmt.Sprintf("cluster-%s", cluster.Name), v1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			_, err = clientset.CoreV1().Secrets("argocd").Create(ctx, clusterSecret, v1.CreateOptions{})
			if err != nil {
				log.Error(err, "unable to create argocd secret")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{Requeue: true}, err
	}

	// if secret does note exist, update it with the above data no matter
	// what information is currently in the secret.
	if _, err := clientset.CoreV1().Secrets("argocd").Update(ctx, clusterSecret, v1.UpdateOptions{}); err != nil {
		log.Error(err, "unable to update argocd secret")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&capi.Cluster{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
