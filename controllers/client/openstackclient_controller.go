/*
Copyright 2023.
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

package client

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keystonev1 "github.com/openstack-k8s-operators/keystone-operator/api/v1beta1"
	"github.com/openstack-k8s-operators/lib-common/modules/common"
	condition "github.com/openstack-k8s-operators/lib-common/modules/common/condition"
	"github.com/openstack-k8s-operators/lib-common/modules/common/configmap"
	"github.com/openstack-k8s-operators/lib-common/modules/common/env"
	helper "github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	common_rbac "github.com/openstack-k8s-operators/lib-common/modules/common/rbac"
	"github.com/openstack-k8s-operators/lib-common/modules/common/secret"
	"github.com/openstack-k8s-operators/lib-common/modules/common/util"
	clientv1 "github.com/openstack-k8s-operators/openstack-operator/apis/client/v1beta1"
	"github.com/openstack-k8s-operators/openstack-operator/pkg/openstackclient"
)

// OpenStackClientReconciler reconciles a OpenStackClient object
type OpenStackClientReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Kclient kubernetes.Interface
	Log     logr.Logger
}

//+kubebuilder:rbac:groups=client.openstack.org,resources=openstackclients,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=client.openstack.org,resources=openstackclients/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=client.openstack.org,resources=openstackclients/finalizers,verbs=update
//+kubebuilder:rbac:groups=keystone.openstack.org,resources=keystoneapis,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;
// service account, role, rolebinding
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=roles,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=rolebindings,verbs=get;list;watch;create;update
// service account permissions that are needed to grant permission to the above
// +kubebuilder:rbac:groups="security.openshift.io",resourceNames=anyuid,resources=securitycontextconstraints,verbs=use
// +kubebuilder:rbac:groups="",resources=pods,verbs=create;delete;get;list;patch;update;watch

// Reconcile -
func (r *OpenStackClientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, _err error) {
	_ = r.Log.WithValues("openstackclient", req.NamespacedName)

	instance := &clientv1.OpenStackClient{}
	err := r.Client.Get(context.TODO(), req.NamespacedName, instance)
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			r.Log.Info("OpenStackClient CR not found", "Name", instance.Name, "Namespace", instance.Namespace)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	r.Log.Info("OpenStackClient CR values", "Name", instance.Name, "Namespace", instance.Namespace, "Secret", instance.Spec.OpenStackConfigSecret, "Image", instance.Spec.ContainerImage)

	instance.Status.Conditions = condition.Conditions{}
	cl := condition.CreateList(
		condition.UnknownCondition(clientv1.OpenStackClientReadyCondition, condition.InitReason, clientv1.OpenStackClientReadyInitMessage),
		// service account, role, rolebinding conditions
		condition.UnknownCondition(condition.ServiceAccountReadyCondition, condition.InitReason, condition.ServiceAccountReadyInitMessage),
		condition.UnknownCondition(condition.RoleReadyCondition, condition.InitReason, condition.RoleReadyInitMessage),
		condition.UnknownCondition(condition.RoleBindingReadyCondition, condition.InitReason, condition.RoleBindingReadyInitMessage),
	)
	instance.Status.Conditions.Init(&cl)

	helper, err := helper.NewHelper(
		instance,
		r.Client,
		r.Kclient,
		r.Scheme,
		r.Log,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always patch the instance status when exiting this function so we can persist any changes.
	defer func() {
		// update the Ready condition based on the sub conditions
		if instance.Status.Conditions.AllSubConditionIsTrue() {
			instance.Status.Conditions.MarkTrue(
				condition.ReadyCondition, condition.ReadyMessage)
		} else {
			// something is not ready so reset the Ready condition
			instance.Status.Conditions.MarkUnknown(
				condition.ReadyCondition, condition.InitReason, condition.ReadyInitMessage)
			// and recalculate it based on the state of the rest of the conditions
			instance.Status.Conditions.Set(
				instance.Status.Conditions.Mirror(condition.ReadyCondition))
		}
		err := helper.PatchInstance(ctx, instance)
		if err != nil {
			_err = err
			return
		}
	}()

	// Service account, role, binding
	rbacRules := []rbacv1.PolicyRule{
		{
			APIGroups:     []string{"security.openshift.io"},
			ResourceNames: []string{"anyuid"},
			Resources:     []string{"securitycontextconstraints"},
			Verbs:         []string{"use"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"create", "get", "list", "watch", "update", "patch", "delete"},
		},
	}
	rbacResult, err := common_rbac.ReconcileRbac(ctx, helper, instance, rbacRules)
	if err != nil {
		return rbacResult, err
	} else if (rbacResult != ctrl.Result{}) {
		return rbacResult, nil
	}

	//
	// Validate that keystoneAPI is up
	//
	keystoneAPI, err := keystonev1.GetKeystoneAPI(ctx, helper, instance.Namespace, map[string]string{})
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			instance.Status.Conditions.Set(condition.FalseCondition(
				clientv1.OpenStackClientReadyCondition,
				condition.RequestedReason,
				condition.SeverityInfo,
				clientv1.OpenStackClientKeystoneWaitingMessage))
			r.Log.Info("KeystoneAPI not found!")
			return ctrl.Result{RequeueAfter: time.Duration(5) * time.Second}, nil
		}
		instance.Status.Conditions.Set(condition.FalseCondition(
			clientv1.OpenStackClientReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			clientv1.OpenStackClientReadyErrorMessage,
			err.Error()))
		return ctrl.Result{}, err
	}
	if !keystoneAPI.IsReady() {
		instance.Status.Conditions.Set(condition.FalseCondition(
			clientv1.OpenStackClientReadyCondition,
			condition.RequestedReason,
			condition.SeverityInfo,
			clientv1.OpenStackClientKeystoneWaitingMessage))
		r.Log.Info("KeystoneAPI not yet ready")
		return ctrl.Result{RequeueAfter: time.Duration(5) * time.Second}, nil
	}

	clientLabels := map[string]string{
		common.AppSelector: "openstackclient",
	}

	configVars := make(map[string]env.Setter)

	_, configMapHash, err := configmap.GetConfigMapAndHashWithName(ctx, helper, *instance.Spec.OpenStackConfigMap, instance.Namespace)
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			instance.Status.Conditions.Set(condition.FalseCondition(
				clientv1.OpenStackClientReadyCondition,
				condition.RequestedReason,
				condition.SeverityInfo,
				clientv1.OpenStackClientConfigMapWaitingMessage))
			return ctrl.Result{RequeueAfter: time.Duration(10) * time.Second}, nil
		}
		instance.Status.Conditions.Set(condition.FalseCondition(
			clientv1.OpenStackClientReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			clientv1.OpenStackClientReadyErrorMessage,
			err.Error()))
		return ctrl.Result{}, err
	}
	configVars[*instance.Spec.OpenStackConfigMap] = env.SetValue(configMapHash)

	_, secretHash, err := secret.GetSecret(ctx, helper, *instance.Spec.OpenStackConfigSecret, instance.Namespace)
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			instance.Status.Conditions.Set(condition.FalseCondition(
				clientv1.OpenStackClientReadyCondition,
				condition.RequestedReason,
				condition.SeverityInfo,
				clientv1.OpenStackClientSecretWaitingMessage))
			return ctrl.Result{RequeueAfter: time.Duration(10) * time.Second}, nil
		}
		instance.Status.Conditions.Set(condition.FalseCondition(
			clientv1.OpenStackClientReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			clientv1.OpenStackClientReadyErrorMessage,
			err.Error()))
		return ctrl.Result{}, err
	}
	configVars[*instance.Spec.OpenStackConfigSecret] = env.SetValue(secretHash)

	if instance.Spec.CaSecretName != "" {
		_, secretHash, err = secret.GetSecret(ctx, helper, instance.Spec.CaSecretName, instance.Namespace)
		if err != nil {
			if k8s_errors.IsNotFound(err) {
				instance.Status.Conditions.Set(condition.FalseCondition(
					clientv1.OpenStackClientReadyCondition,
					condition.RequestedReason,
					condition.SeverityInfo,
					clientv1.OpenStackClientSecretWaitingMessage))
				return ctrl.Result{RequeueAfter: time.Duration(10) * time.Second}, nil
			}
			instance.Status.Conditions.Set(condition.FalseCondition(
				clientv1.OpenStackClientReadyCondition,
				condition.ErrorReason,
				condition.SeverityWarning,
				clientv1.OpenStackClientReadyErrorMessage,
				err.Error()))
			return ctrl.Result{}, err
		}
		configVars[instance.Spec.CaSecretName] = env.SetValue(secretHash)
	}

	configVarsHash, err := util.HashOfInputHashes(configVars)
	if err != nil {
		return ctrl.Result{}, err
	}

	pod, err := openstackclient.ClientPod(ctx, instance, helper, clientLabels, configVarsHash)
	if err != nil {
		return ctrl.Result{}, err
	}

	op, err := controllerutil.CreateOrPatch(ctx, r.Client, pod, func() error {
		pod.Spec.Containers[0].Image = instance.Spec.ContainerImage
		err := controllerutil.SetControllerReference(instance, pod, r.Scheme)
		if err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		instance.Status.Conditions.Set(condition.FalseCondition(
			clientv1.OpenStackClientReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			condition.DeploymentReadyErrorMessage,
			err.Error()))
		return ctrl.Result{}, err
	}

	if op != controllerutil.OperationResultNone {
		util.LogForObject(
			helper,
			fmt.Sprintf("Pod %s successfully reconciled - operation: %s", pod.Name, string(op)),
			instance,
		)
	}

	instance.Status.Conditions.MarkTrue(
		clientv1.OpenStackClientReadyCondition,
		clientv1.OpenStackClientReadyMessage,
	)

	return ctrl.Result{}, nil

}

// SetupWithManager sets up the controller with the Manager.
func (r *OpenStackClientReconciler) SetupWithManager(mgr ctrl.Manager) error {

	return ctrl.NewControllerManagedBy(mgr).
		For(&clientv1.OpenStackClient{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Complete(r)
}
