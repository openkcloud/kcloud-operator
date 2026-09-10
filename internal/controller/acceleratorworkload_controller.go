// ============================================================
// acceleratorworkload_controller.go: AcceleratorWorkload 컨트롤러
// 상세: 추상 워크로드의 최종 권위 판정. admission webhook 은 failurePolicy=Ignore 라 우회될
// 수 있고 승인 이후 클러스터 capability 가 바뀔 수도 있으므로, 같은 intent.Translate 를 다시
// 돌려 결과를 status 에 남긴다. 성공하면 Deployment 하나를 소유하고, 거절되면 Deployment 를
// 만들지 않고 Condition=False 로 사유를 남긴다(실행 중인 것을 강제로 죽이지는 않는다 —
// 광고가 사라지면 Pod 은 자연히 Pending 이 되고, 그 판단은 운영자 몫이다).
// 생성일: 2026-07-30 | 수정일: 2026-07-30
// ============================================================
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/intent"
	"kcloud-operator/internal/upgrade"
)

// podInjectLabel 은 기존 PodMutator(internal/webhook.InjectLabel)의 opt-in 라벨이다.
// 렌더한 Pod 에 붙여 runtimeClass 주입 경로를 재사용한다(새 mutating 경로를 만들지 않는다).
const podInjectLabel = "kcloud.ai/inject"

// awWorkloadLabel 은 렌더한 Deployment/Pod 를 CR 로 되짚는 selector 다.
const awWorkloadLabel = "kcloud.ai/accelerator-workload"

// awRequeueInterval 은 클러스터 capability 가 바뀌면 판정도 바뀌므로 주기적으로 다시 본다.
const awRequeueInterval = 2 * time.Minute

// errDeploymentNotOwned 는 같은 이름의 Deployment 가 남의 것일 때다. 뺏지 않는다.
var errDeploymentNotOwned = errors.New("deployment is not owned by this AcceleratorWorkload")

// AcceleratorWorkloadReconciler 는 추상 워크로드를 Deployment 로 실현한다.
type AcceleratorWorkloadReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=npu.ai,resources=acceleratorclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=npu.ai,resources=acceleratorworkloads,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=npu.ai,resources=acceleratorworkloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch

func (r *AcceleratorWorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var aw npuv1alpha1.AcceleratorWorkload
	if err := r.Get(ctx, req.NamespacedName, &aw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var class npuv1alpha1.AcceleratorClass
	if err := r.Get(ctx, types.NamespacedName{Name: aw.Spec.Accelerator.Class}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			return r.rejected(ctx, &aw, npuv1alpha1.AWReasonClassNotFound, "",
				fmt.Sprintf("AcceleratorClass %q does not exist", aw.Spec.Accelerator.Class))
		}
		return ctrl.Result{}, err
	}

	snap, err := intent.Load(ctx, r.Client)
	if err != nil {
		// 재시도해도 같은 결과인 실패(주로 RBAC)를 백오프에 묻어 두면 CR 은 영영 빈 status 로 남는다.
		if nonRetryableAPIError(err) {
			return r.rejected(ctx, &aw, npuv1alpha1.AWReasonCapabilityUnverified, "",
				fmt.Sprintf("cluster capability cannot be read, so nothing can be confirmed: %v", err))
		}
		return ctrl.Result{}, err
	}
	res, err := intent.Translate(intent.BuildRequest(&aw, &class), snap)
	if err != nil {
		var rj *intent.Reject
		if errors.As(err, &rj) {
			return r.rejected(ctx, &aw, rj.Reason, rj.Axis, rj.Error())
		}
		return ctrl.Result{}, err
	}

	dep := renderDeployment(&aw, res)
	if err := ctrl.SetControllerReference(&aw, dep, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	live, err := r.applyDeployment(ctx, dep)
	if err != nil {
		if errors.Is(err, errDeploymentNotOwned) {
			// 번역 자체는 성공했다 — Rejected 로 뭉개지 않고 실현 실패 사유를 따로 남긴다.
			return ctrl.Result{RequeueAfter: awRequeueInterval},
				r.translated(ctx, &aw, res, nil, npuv1alpha1.AWReasonDeploymentConflict, err.Error())
		}
		// Invalid/Forbidden/quota/terminating namespace 는 재시도해도 같다. 백오프로 영영 돌면서
		// status 를 비워 두면 사용자는 operator 로그 말고는 볼 것이 없다.
		if nonRetryableAPIError(err) {
			return ctrl.Result{RequeueAfter: awRequeueInterval},
				r.translated(ctx, &aw, res, nil, npuv1alpha1.AWReasonDeploymentInvalid, err.Error())
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: awRequeueInterval}, r.translated(ctx, &aw, res, live, "", "")
}

// nonRetryableAPIError 는 같은 입력으로 다시 불러도 같은 결과가 나오는 API 실패다.
// (Invalid=잘못된 리소스명 등, Forbidden=RBAC·quota·terminating namespace, BadRequest=요청 형식.)
// applyDeployment/Load 가 %w 로 감싸도 apierrors 가 errors.As 로 풀어 본다.
func nonRetryableAPIError(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsBadRequest(err)
}

// applyDeployment 는 없으면 만들고, 우리가 소유한 것이면 우리가 정하는 필드만 맞춘다.
// 바뀐 게 없으면 쓰지 않는다 — 재조정마다 Update 를 날리면 Pod 이 계속 다시 뜬다.
func (r *AcceleratorWorkloadReconciler) applyDeployment(ctx context.Context, want *appsv1.Deployment) (*appsv1.Deployment, error) {
	var live appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Name: want.Name, Namespace: want.Namespace}, &live)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, want); err != nil {
			return nil, fmt.Errorf("create deployment %s/%s: %w", want.Namespace, want.Name, err)
		}
		return want, nil
	}
	if err != nil {
		return nil, err
	}
	owner := metav1.GetControllerOf(&live)
	if owner == nil || owner.UID != want.OwnerReferences[0].UID {
		return nil, fmt.Errorf("deployment %s/%s already exists and is not controlled by this workload: %w",
			want.Namespace, want.Name, errDeploymentNotOwned)
	}
	merged := mergeDeployment(&live, want)
	if apiequality.Semantic.DeepEqual(live.Spec, merged.Spec) && apiequality.Semantic.DeepEqual(live.Labels, merged.Labels) {
		return &live, nil
	}
	if err := r.Update(ctx, merged); err != nil {
		return nil, fmt.Errorf("update deployment %s/%s: %w", want.Namespace, want.Name, err)
	}
	return merged, nil
}

// mergeDeployment 는 우리가 소유하는 필드만 live 위에 얹는다. live.Spec 을 통째로 갈아끼우면
// API server 가 채워 넣은 기본값(dnsPolicy·strategy·imagePullPolicy…)이 매번 지워졌다 다시
// 채워져 "항상 다름" 이 되고, 그 결과 재조정마다 rolling restart 가 난다.
func mergeDeployment(live, want *appsv1.Deployment) *appsv1.Deployment {
	out := live.DeepCopy()
	// 라벨은 우리 키만 맞춘다. 통째로 갈아끼우면 운영자가 붙인 quiesce opt-in
	// (npu.ai/quiesce-on-driver-upgrade)이 2분 안에 지워져 이 워크로드는 영영 quiesce 대상이 못 된다.
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	for k, v := range want.Labels {
		out.Labels[k] = v
	}
	// driver upgrade quiesce 가 scale=0 으로 내려둔 동안에는 replicas 를 되돌리지 않는다.
	// 되돌리면 rmmod 창 안에서 GPU 를 쥔 Pod 이 다시 뜬다. 이 annotation 은 quiesce 가 걸 때
	// 쓰고 복구할 때 지우므로(upgrade.QuiesceLabeledDeployments/restore), 창이 닫히면 저절로 풀린다.
	if _, quiesced := out.Annotations[upgrade.QuiesceReplicasBackupAnnotation]; !quiesced {
		out.Spec.Replicas = want.Spec.Replicas
	}
	out.Spec.Selector = want.Spec.Selector
	out.Spec.Template.Labels = want.Spec.Template.Labels
	out.Spec.Template.Spec.Affinity = want.Spec.Template.Spec.Affinity
	// 이 필드도 우리 소유다 — 안 맞추면 이 수정 이전에 만들어진 Deployment 는 영영 runtimeClass 없이 남는다.
	out.Spec.Template.Spec.RuntimeClassName = want.Spec.Template.Spec.RuntimeClassName
	wc := want.Spec.Template.Spec.Containers[0]
	if len(out.Spec.Template.Spec.Containers) != 1 {
		out.Spec.Template.Spec.Containers = want.Spec.Template.Spec.Containers
		return out
	}
	c := &out.Spec.Template.Spec.Containers[0]
	c.Name, c.Image, c.Command, c.Args, c.Env, c.Resources = wc.Name, wc.Image, wc.Command, wc.Args, wc.Env, wc.Resources
	return out
}

func (r *AcceleratorWorkloadReconciler) rejected(ctx context.Context, aw *npuv1alpha1.AcceleratorWorkload, reason, axis, message string) (ctrl.Result, error) {
	// condition 을 덮기 전에 직전 사유를 본다 — 아래 이벤트 발행이 "바뀐 것만" 이어야 한다.
	changed := conditionReasonChanged(aw, npuv1alpha1.AWCondTranslated, reason)
	aw.Status.ObservedGeneration = aw.Generation
	aw.Status.Phase = npuv1alpha1.AWPhaseRejected
	// 더 이상 성립하지 않는 번역 결과를 남겨두면 사용자가 그 리소스명을 아직 유효한 것으로 읽는다.
	aw.Status.Resolved = nil
	// 축을 구조로 남긴다. condition message 는 그대로 둔다 — 사람이 읽는 경로이자
	// 구버전 객체를 읽는 소비자의 폴백이다.
	aw.Status.Rejection = &npuv1alpha1.WorkloadRejection{Axis: axis, Reason: reason, Message: message}
	apimeta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
		Type: npuv1alpha1.AWCondTranslated, Status: metav1.ConditionFalse,
		Reason: reason, Message: message, ObservedGeneration: aw.Generation,
	})
	// 거절된 워크로드는 Ready 가 아니다. spec 이 그대로면 observedGeneration 으로는 낡음을
	// 알 수 없으므로, 예전 조정이 남긴 WorkloadReady=True 를 여기서 직접 내려야 한다.
	apimeta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
		Type: npuv1alpha1.AWCondWorkloadReady, Status: metav1.ConditionFalse,
		Reason: reason, Message: message, ObservedGeneration: aw.Generation,
	})
	// 사유가 그대로면 이벤트를 다시 내지 않는다 — 2분 주기 × 영구 거절이면 하루 ~720개가 쌓여
	// describe 를 못 읽게 만들고 etcd 를 흔든다.
	if r.Recorder != nil && changed {
		r.Recorder.Event(aw, corev1.EventTypeWarning, reason, message)
	}
	// 클러스터가 바뀌면 판정도 바뀌므로 주기적으로 다시 본다(에러로 올리지 않는다 — 사용자 입력 문제다).
	return ctrl.Result{RequeueAfter: awRequeueInterval}, r.Status().Update(ctx, aw)
}

// translated 는 번역 성공 status 를 쓴다. blockedReason 이 비어 있지 않으면 Deployment 를
// 실현하지 못한 것이므로 WorkloadReady 를 그 사유로 False 로 남긴다.
func (r *AcceleratorWorkloadReconciler) translated(ctx context.Context, aw *npuv1alpha1.AcceleratorWorkload,
	res *intent.Result, dep *appsv1.Deployment, blockedReason, blockedMessage string) error {
	// 실현 실패 사유는 WorkloadReady 에 실리므로 그 condition 의 직전 사유와 비교한다
	// (Translated 는 이 경로에서 언제나 AWReasonTranslated 라 변화를 담지 못한다).
	blockedChanged := conditionReasonChanged(aw, npuv1alpha1.AWCondWorkloadReady, blockedReason)
	aw.Status.ObservedGeneration = aw.Generation
	resolved := *res
	aw.Status.Resolved = &resolved
	// 번역이 성공했으면 이전 거절을 지운다(남으면 고친 뒤에도 거절로 보인다).
	aw.Status.Rejection = nil
	apimeta.SetStatusCondition(&aw.Status.Conditions, metav1.Condition{
		Type: npuv1alpha1.AWCondTranslated, Status: metav1.ConditionTrue,
		Reason: npuv1alpha1.AWReasonTranslated, Message: res.Explanation, ObservedGeneration: aw.Generation,
	})

	ready := metav1.Condition{Type: npuv1alpha1.AWCondWorkloadReady, Status: metav1.ConditionFalse,
		Reason: npuv1alpha1.AWReasonWorkloadNotReady, Message: "no pod is ready yet", ObservedGeneration: aw.Generation}
	aw.Status.Phase = npuv1alpha1.AWPhaseTranslated
	switch {
	case blockedReason != "":
		ready.Reason, ready.Message = blockedReason, blockedMessage
		if r.Recorder != nil && blockedChanged {
			r.Recorder.Event(aw, corev1.EventTypeWarning, blockedReason, blockedMessage)
		}
	case dep != nil && dep.Status.ReadyReplicas > 0:
		aw.Status.Phase = npuv1alpha1.AWPhaseRunning
		ready.Status, ready.Reason = metav1.ConditionTrue, npuv1alpha1.AWReasonWorkloadReady
		ready.Message = fmt.Sprintf("%d/%d pods ready", dep.Status.ReadyReplicas, dep.Status.Replicas)
	}
	apimeta.SetStatusCondition(&aw.Status.Conditions, ready)
	return r.Status().Update(ctx, aw)
}

// conditionReasonChanged 는 그 condition 의 사유가 지금 쓰려는 것과 다른지 본다.
// 아직 그 condition 이 없으면(첫 조정) 바뀐 것으로 본다 — 첫 사유는 반드시 이벤트로 알린다.
func conditionReasonChanged(aw *npuv1alpha1.AcceleratorWorkload, condType, reason string) bool {
	cur := apimeta.FindStatusCondition(aw.Status.Conditions, condType)
	return cur == nil || cur.Reason != reason
}

// renderDeployment 는 번역 결과를 그대로 Pod 사양으로 옮긴다.
// 노드 후보는 nodeAffinity 로 박는다 — RNGD 처럼 파티션을 flat 광고하는 벤더는 리소스명만으로
// 올바른 노드를 고를 수 없기 때문이다.
func renderDeployment(aw *npuv1alpha1.AcceleratorWorkload, res *intent.Result) *appsv1.Deployment {
	replicas := int32(1)
	if aw.Spec.Workload.Replicas != nil {
		replicas = *aw.Spec.Workload.Replicas
	}
	selector := map[string]string{awWorkloadLabel: aw.Name}
	podLabels := map[string]string{
		awWorkloadLabel:                aw.Name,
		"app.kubernetes.io/managed-by": "kcloud-operator",
		podInjectLabel:                 labelValueTrue,
	}
	limits := corev1.ResourceList{
		corev1.ResourceName(res.ResourceName): *resource.NewQuantity(int64(res.Quantity), resource.DecimalSI),
	}
	// NVIDIA 는 runtimeClass 를 여기서 직접 박는다. 노드의 containerd 기본 런타임은 runc 이고
	// nvidia hook 은 이 핸들러에서만 돌므로, 없으면 컨테이너는 /dev/nvidia* 없이 뜬다 —
	// 그런데도 Pod 은 Ready 가 되어 status 가 Running 이라고 거짓말한다. PodMutator 는
	// webhook.enabled=false(차트 기본값)면 아예 불리지 않으므로 거기 기댈 수 없다.
	// podInjectLabel 은 계속 붙인다 — webhook 이 켜져 있으면 무해한 이중 방어다.
	var runtimeClass *string
	if res.Vendor == vendorNvidia {
		rc := vendorNvidia
		runtimeClass = &rc
	}
	var affinity *corev1.Affinity
	if len(res.Nodes) > 0 {
		affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: res.Nodes,
					}},
				}},
			},
		}}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: aw.Name, Namespace: aw.Namespace, Labels: selector},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					Affinity:         affinity,
					RuntimeClassName: runtimeClass,
					Containers: []corev1.Container{{
						Name:      "workload",
						Image:     aw.Spec.Workload.Image,
						Command:   aw.Spec.Workload.Command,
						Args:      aw.Spec.Workload.Args,
						Env:       aw.Spec.Workload.Env,
						Resources: corev1.ResourceRequirements{Limits: limits},
					}},
				},
			},
		},
	}
}

func (r *AcceleratorWorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&npuv1alpha1.AcceleratorWorkload{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
