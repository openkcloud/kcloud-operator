package controller

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
)

const (
	annotationInstallAttempt = "npu.ai/install-attempt"
	maxInstallRetries        = 5
	baseCooldownMinutes      = 10
)

// RBAC 권한 (노드 패치 + CRD 접근)
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=npu.ai,resources=nodedevicereports;nodedevicereports/status;driverinstallpolicies,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

type DriverInstallReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// 특정 벤더에 대해, 노드 라벨이 설치 허용 조건을 만족하는지 검사
func allowInstallForVendor(node *corev1.Node) bool {
	if node == nil {
		return false
	}

	// 공통 드라이버 설치 허용 라벨
	if node.Labels["npu.driver.install"] == "true" {
		return true
	}
	return false
}

func (r *DriverInstallReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1) NodeDeviceReport 조회
	var rep npuv1alpha1.NodeDeviceReport
	if err := r.Get(ctx, req.NamespacedName, &rep); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	nodeName := rep.Spec.NodeName
	if nodeName == "" {
		// 잘못된 리포트
		return ctrl.Result{}, nil
	}
	ns := r.jobNamespace()

	// 1-1) 이 노드용 Job 상태를 2-pass로 스캔 (리스트 순서에 의존하지 않음)
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs,
		client.InNamespace(ns),
		client.MatchingLabels{"app": "npu-op-installer", "node": nodeName},
	); err == nil {
		hasActive := false
		hasSucceeded := false
		var latestFailedCooldown *time.Time
		for i := range jobs.Items {
			j := &jobs.Items[i]
			if j.Status.Active > 0 {
				hasActive = true
			}
			if j.Status.Succeeded > 0 {
				hasSucceeded = true
			}
			if !hasSucceeded && j.Status.Succeeded == 0 && isTerminal(j) {
				cooldownBase := j.ObjectMeta.CreationTimestamp.Time
				if j.Status.CompletionTime != nil {
					cooldownBase = j.Status.CompletionTime.Time
				}
				if latestFailedCooldown == nil || cooldownBase.After(*latestFailedCooldown) {
					latestFailedCooldown = &cooldownBase
				}
			}
		}
		// 우선순위: active > succeeded > failed cooldown
		if hasActive {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if hasSucceeded {
			logger.Info("Succeeded install Job exists, skipping", "node", nodeName)
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}
		if latestFailedCooldown != nil {
			// 지수 백오프: attempt에 따라 10m, 20m, 40m, 80m, 160m
			attempt := r.getLastAttempt(ctx, ns, nodeName)
			cooldown := exponentialCooldown(attempt)
			if time.Since(*latestFailedCooldown) < cooldown {
				return ctrl.Result{
					RequeueAfter: time.Until(latestFailedCooldown.Add(cooldown)),
				}, nil
			}
		}
	} else {
		// 리스트 실패시 보수적으로 재시도
		logger.Error(err, "failed to list driverinstall jobs", "node", nodeName)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// 2) DriverInstallPolicy 목록 조회
	var pols npuv1alpha1.DriverInstallPolicyList
	if err := r.List(ctx, &pols); err != nil {
		return ctrl.Result{}, err
	}

	// 3) 모든 디바이스 드라이버가 로드되었으면 라벨링 + 버전 불일치 확인
	if allLoaded(rep) {
		r.Recorder.Eventf(&rep, corev1.EventTypeNormal, "DriversLoaded", "All device drivers loaded on node %s", nodeName)
		labels := map[string]string{}
		for _, d := range rep.Status.Devices {
			if d.Vendor != "" && d.Model != "" {
				labels[fmt.Sprintf("accelerator.%s.%s", d.Vendor, d.Model)] = "true"
			}
		}
		if len(labels) > 0 {
			if err := r.ensureNodeLabels(ctx, nodeName, labels, rep); err != nil {
				return ctrl.Result{}, err
			}
		}

		// 업그레이드 완료 후 uncordon
		if err := r.uncordonNode(ctx, nodeName); err != nil {
			logger.Error(err, "failed to uncordon node after upgrade", "node", nodeName)
		}

		// 버전 불일치 확인
		needUpgrade, targetDevice := r.checkVersionMismatch(ctx, &rep, &pols)
		if !needUpgrade {
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}

		// upgradePolicy 확인
		p := matchPolicy(&pols, targetDevice)
		if p == nil || p.Spec.UpgradePolicy == nil || !p.Spec.UpgradePolicy.AutoUpgrade {
			logger.Info("Version mismatch detected but autoUpgrade not enabled", "node", nodeName)
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}

		logger.Info("Driver version upgrade needed", "node", nodeName,
			"current", targetDevice.DriverVersion, "desired", p.Spec.Driver.Version)

		// npu.driver.install 라벨 게이트 확인
		var node corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
			return ctrl.Result{}, err
		}
		if !allowInstallForVendor(&node) {
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}
		// DIP.spec.nodeSelector 매칭 확인 (detector 오탐 2차 차단)
		if !nodeMatchesPolicy(&node, p) {
			logger.Info("Node does not match DriverInstallPolicy.nodeSelector, skipping upgrade",
				"node", nodeName, "policy", p.Name, "policyNodeSelector", p.Spec.NodeSelector)
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}

		// GPU 사용 Pod 확인 + drain 로직
		if p.Spec.UpgradePolicy.DrainEnabled {
			hasPods, err := r.hasDeviceWorkloads(ctx, nodeName, p.Spec.Vendor)
			if err != nil {
				return ctrl.Result{}, err
			}
			if hasPods {
				canDrain := r.canDrainNode(ctx, nodeName)
				if !canDrain && !p.Spec.UpgradePolicy.ForceUpgrade {
					// force annotation 확인
					forceAnnotation := false
					var policies npuv1alpha1.NPUClusterPolicyList
					if err := r.List(ctx, &policies); err == nil {
						for _, pol := range policies.Items {
							if pol.Annotations != nil && pol.Annotations["npu.ai/force-upgrade"] == "true" {
								forceAnnotation = true
								break
							}
						}
					}
					if !forceAnnotation {
						r.Recorder.Eventf(&rep, corev1.EventTypeWarning, "DrainBlocked",
							"Node %s has device workloads but cannot drain (no alternative nodes). Use --force or set forceUpgrade=true", nodeName)
						logger.Info("Drain blocked, waiting for force", "node", nodeName)
						return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
					}
				}
				// cordon + drain
				if err := r.cordonNode(ctx, nodeName); err != nil {
					logger.Error(err, "failed to cordon node", "node", nodeName)
				}
				r.Recorder.Eventf(&rep, corev1.EventTypeNormal, "UpgradeDrain", "Draining node %s for driver upgrade", nodeName)
			}
		}

		// 기존 성공 Job이 있으면 삭제 (업그레이드를 위해)
		jobName := fmt.Sprintf("npu-op-installer-%s", sanitize(nodeName))
		var existingJob batchv1.Job
		if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, &existingJob); err == nil {
			if err := r.Delete(ctx, &existingJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// 새 installer Job 생성
		job := r.renderInstallJob(nodeName, p, &rep)
		if err := r.Create(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(&rep, corev1.EventTypeNormal, "UpgradeStarted",
			"Started driver upgrade on node %s: %s -> %s", nodeName, targetDevice.DriverVersion, p.Spec.Driver.Version)

		return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	}

	// 4) driverLoaded=false 대상이 있는지 판단
	needInstall := false
	for _, d := range rep.Status.Devices {
		if !d.DriverLoaded {
			if matchPolicy(&pols, d) != nil {
				needInstall = true
				break
			}
		}
	}

	if !needInstall {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// --- 노드 역할/라벨 기반 게이트 ---
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err == nil {
		// 컨트롤 플레인/마스터는 스킵
		if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}
		if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}
	}

	// 5) 설치 Job 생성/재생성
	jobName := fmt.Sprintf("npu-op-installer-%s", sanitize(nodeName))

	var job batchv1.Job
	getErr := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, &job)
	if getErr == nil {
		// 성공 완료된 Job은 삭제하지 않음 — TTL(10분)에 맡기고 재생성 안 함
		if job.Status.Succeeded > 0 {
			logger.Info("Succeeded install Job exists, no action needed", "node", nodeName)
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}
		if isTerminal(&job) {
			attempt := getAttemptCount(&job)

			// 실패한 Job 삭제
			logger.Info("Deleting failed install Job for retry", "node", nodeName, "job", jobName, "attempt", attempt)
			if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}

			// 재시도 횟수 초과 확인
			if attempt >= maxInstallRetries {
				logger.Info("Max install retries exceeded, marking InstallFailed",
					"node", nodeName, "attempts", attempt)
				r.Recorder.Eventf(&rep, corev1.EventTypeWarning, "InstallFailed",
					"Driver installation failed after %d attempts on node %s", attempt, nodeName)
				if err := r.setInstallFailedCondition(ctx, &rep, attempt); err != nil {
					logger.Error(err, "failed to set InstallFailed condition")
				}
				return ctrl.Result{RequeueAfter: 1 * time.Hour}, nil
			}

			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}
	if apierrors.IsNotFound(getErr) {
		p := matchPolicy(&pols, firstUnloaded(rep))
		if p == nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// DriverInstallPolicy.spec.nodeSelector 와 노드 라벨 매칭 확인.
		// detector 가 실제 하드웨어가 없는 노드에서 벤더를 오보고(예: npu 모듈 leftover)
		// 하더라도 policy 의 nodeSelector 로 2차 차단한다.
		if !nodeMatchesPolicy(&node, p) {
			logger.Info("Node does not match DriverInstallPolicy.nodeSelector, skipping install",
				"node", nodeName, "policy", p.Name, "policyNodeSelector", p.Spec.NodeSelector)
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}

		// InstallFailed 상태인 경우 Job 생성 중단
		if hasInstallFailedCondition(&rep) {
			logger.Info("Skipping Job creation: InstallFailed condition set", "node", nodeName)
			return ctrl.Result{RequeueAfter: 1 * time.Hour}, nil
		}

		// 벤더별 라벨 게이트: 해당 노드가 벤더 라벨을 만족하지 않으면 설치 금지
		// (위에서 node를 읽어왔으니 그대로 사용)
		if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err == nil {
			if !allowInstallForVendor(&node) {
				// 라벨이 준비되면 다시 시도
				return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
			}
		}

		nextAttempt := r.getLastAttempt(ctx, ns, nodeName) + 1
		job = *r.renderInstallJob(nodeName, p, &rep)
		if job.Annotations == nil {
			job.Annotations = map[string]string{}
		}
		job.Annotations[annotationInstallAttempt] = strconv.Itoa(nextAttempt)

		if err := r.Create(ctx, &job); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(&rep, corev1.EventTypeNormal, "JobCreated", "Created driver install Job for node %s with policy %s (attempt %d)", nodeName, p.Name, nextAttempt)
		logger.Info("Created driver install Job", "node", nodeName, "policy", p.Name, "attempt", nextAttempt)
	} else if getErr != nil {
		return ctrl.Result{}, getErr
	}

	// 6) 다음 주기
	return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
}

func (r *DriverInstallReconciler) jobNamespace() string {
	// manager 네임스페이스와 맞추는 것이 일반적이나, 여기서는 kube-system 고정
	return "kube-system"
}

func (r *DriverInstallReconciler) renderInstallJob(nodeName string, p *npuv1alpha1.DriverInstallPolicy, rep *npuv1alpha1.NodeDeviceReport) *batchv1.Job {
	backoff := int32(0)
	ttl := int32(600) // 완료 10분 후 자동 삭제

	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("npu-op-installer-%s", sanitize(nodeName)), // 고정 이름 전략
			Namespace: r.jobNamespace(),
			Labels: map[string]string{
				"app":    "npu-op-installer",
				"node":   nodeName,
				"vendor": p.Spec.Vendor,
				"model":  p.Spec.Model,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					// ServiceAccountName: "npu-operator-controller-manager", // 필요 시 사용
					RestartPolicy: corev1.RestartPolicyNever,
					NodeName:      nodeName,            // 지정 노드에 고정
					NodeSelector:  map[string]string{}, // 벤더별 이중 게이트를 위해 추가
					HostPID:       true,
					HostNetwork:   true,
					Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name:  "installer",
						Image: p.Spec.Driver.Image,
						SecurityContext: &corev1.SecurityContext{
							Privileged: boolPtr(true),
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "modules", MountPath: "/lib/modules"},
							{Name: "usr-src", MountPath: "/usr/src"},
							{Name: "var-lib", MountPath: "/var/lib/npu-operator"},
							{Name: "host-etc", MountPath: "/host/etc"},
						},
						Env: []corev1.EnvVar{
							{Name: "TARGET_NODE", Value: nodeName},
							{Name: "REBOOT_STRATEGY", Value: p.Spec.RebootStrategy},
							{Name: "DRIVER_VERSION", Value: p.Spec.Driver.Version},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "modules", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/lib/modules"}}},
						{Name: "usr-src", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/usr/src"}}},
						{Name: "var-lib", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/npu-operator"}}},
						{Name: "host-etc", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc"}}},
					},
				},
			},
		},
	}

	// Furiosa 전용 Secret 마운트
	if strings.EqualFold(p.Spec.Vendor, "furiosa") {
		c := &j.Spec.Template.Spec.Containers[0]
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name: "furiosa-auth", MountPath: "/secrets", ReadOnly: true,
		})
		j.Spec.Template.Spec.Volumes = append(j.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: "furiosa-auth",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "furiosa-apt-auth"},
			},
		})
	}

	// ✅ 벤더별 NodeSelector 이중 안전장치
	if j.Spec.Template.Spec.NodeSelector == nil {
		j.Spec.Template.Spec.NodeSelector = map[string]string{}
	}
	j.Spec.Template.Spec.NodeSelector["npu.driver.install"] = "true"

	return j
}

func (r *DriverInstallReconciler) ensureNodeLabels(ctx context.Context, nodeName string, labels map[string]string, rep npuv1alpha1.NodeDeviceReport) error {
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return err
	}
	// DeepCopy를 수정 전에 생성해야 MergeFrom이 올바른 diff를 계산함
	base := node.DeepCopy()

	patched := false
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	for k, v := range labels {
		if node.Labels[k] != v {
			node.Labels[k] = v
			patched = true
		}
	}
	if ver := matchedDriverVersion(rep); ver != "" {
		k := "driver.furiosa.version" // 필요시 vendor/model 별 분리
		if node.Labels[k] != ver {
			node.Labels[k] = ver
			patched = true
		}
	}
	if !patched {
		return nil
	}
	return r.Patch(ctx, &node, client.MergeFrom(base))
}

func isTerminal(j *batchv1.Job) bool {
	if j.Status.Succeeded > 0 {
		return true
	}
	if j.Status.Failed > 0 {
		// BackoffLimit 소진 판단: BackoffLimit=N이면 N+1번째 실패 시 terminal
		// 예) BackoffLimit=0 → Failed>0(=1)이면 terminal, Failed=0이면 아직 진행 중
		if j.Spec.BackoffLimit != nil && j.Status.Failed > *j.Spec.BackoffLimit {
			return true
		}
	}
	return false
}

func matchedDriverVersion(rep npuv1alpha1.NodeDeviceReport) string {
	for _, d := range rep.Status.Devices {
		if d.DriverLoaded && d.DriverVersion != "" {
			return d.DriverVersion
		}
	}
	return ""
}

func allLoaded(rep npuv1alpha1.NodeDeviceReport) bool {
	loaded := false
	for _, d := range rep.Status.Devices {
		if !d.DriverLoaded {
			return false
		}
		loaded = true
	}
	return loaded // 하나라도 있고 모두 true 여야 true
}

func firstUnloaded(rep npuv1alpha1.NodeDeviceReport) npuv1alpha1.DeviceEntry {
	for _, d := range rep.Status.Devices {
		if !d.DriverLoaded {
			return d
		}
	}
	return npuv1alpha1.DeviceEntry{}
}

func sanitize(s string) string {
	// 노드 이름을 RFC 1123 DNS label 호환 문자열로 변환
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, ".", "-")
	s = strings.ReplaceAll(s, "_", "-")
	// "npu-op-installer-" prefix(17자) + sanitized name ≤ 63자
	if len(s) > 45 {
		s = s[:45]
	}
	s = strings.TrimRight(s, "-")
	return s
}

// nodeMatchesPolicy 는 DriverInstallPolicy.spec.nodeSelector 가 설정된 경우
// 대상 노드의 라벨이 전부 일치하는지 확인한다. nodeSelector 가 비어있으면 true.
func nodeMatchesPolicy(node *corev1.Node, p *npuv1alpha1.DriverInstallPolicy) bool {
	if p == nil || len(p.Spec.NodeSelector) == 0 {
		return true
	}
	if node == nil || node.Labels == nil {
		return false
	}
	for k, v := range p.Spec.NodeSelector {
		if node.Labels[k] != v {
			return false
		}
	}
	return true
}

func matchPolicy(pols *npuv1alpha1.DriverInstallPolicyList, d npuv1alpha1.DeviceEntry) *npuv1alpha1.DriverInstallPolicy {
	for i := range pols.Items {
		p := &pols.Items[i]
		if p.Spec.Driver.Mode == "daemonset" {
			continue
		}
		if p.Spec.Vendor == d.Vendor && p.Spec.Model == d.Model {
			// TODO: 커널/컨테이너런타임 버전 체크 추가 가능
			return p
		}
	}
	return nil
}


// getAttemptCount는 Job annotation에서 재시도 횟수를 읽는다.
func getAttemptCount(j *batchv1.Job) int {
	if j.Annotations == nil {
		return 0
	}
	v, err := strconv.Atoi(j.Annotations[annotationInstallAttempt])
	if err != nil {
		return 0
	}
	return v
}

// exponentialCooldown은 attempt 횟수에 따라 지수 백오프 쿨다운을 반환한다.
func exponentialCooldown(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > maxInstallRetries {
		attempt = maxInstallRetries
	}
	minutes := float64(baseCooldownMinutes) * math.Pow(2, float64(attempt))
	return time.Duration(minutes) * time.Minute
}

// getLastAttempt는 해당 노드의 마지막 재시도 번호를 반환한다.
func (r *DriverInstallReconciler) getLastAttempt(ctx context.Context, ns, nodeName string) int {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs,
		client.InNamespace(ns),
		client.MatchingLabels{"app": "npu-op-installer", "node": nodeName},
	); err == nil {
		maxAttempt := 0
		for _, j := range jobs.Items {
			a := getAttemptCount(&j)
			if a > maxAttempt {
				maxAttempt = a
			}
		}
		if maxAttempt > 0 {
			return maxAttempt
		}
	}
	// NodeDeviceReport conditions에서 마지막 attempt 읽기
	var rep npuv1alpha1.NodeDeviceReport
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &rep); err == nil {
		for _, c := range rep.Status.Conditions {
			if c.Type == "InstallFailed" {
				v, err := strconv.Atoi(c.Message)
				if err == nil {
					return v
				}
			}
		}
	}
	return 0
}

// setInstallFailedCondition은 NodeDeviceReport에 InstallFailed condition을 기록한다.
func (r *DriverInstallReconciler) setInstallFailedCondition(ctx context.Context, rep *npuv1alpha1.NodeDeviceReport, attempt int) error {
	found := false
	for i, c := range rep.Status.Conditions {
		if c.Type == "InstallFailed" {
			rep.Status.Conditions[i].Status = "True"
			rep.Status.Conditions[i].Reason = "MaxRetriesExceeded"
			rep.Status.Conditions[i].Message = strconv.Itoa(attempt)
			found = true
			break
		}
	}
	if !found {
		rep.Status.Conditions = append(rep.Status.Conditions, npuv1alpha1.Condition{
			Type:    "InstallFailed",
			Status:  "True",
			Reason:  "MaxRetriesExceeded",
			Message: strconv.Itoa(attempt),
		})
	}
	return r.Status().Update(ctx, rep)
}

// hasInstallFailedCondition은 NodeDeviceReport에 InstallFailed condition이 있는지 확인한다.
func hasInstallFailedCondition(rep *npuv1alpha1.NodeDeviceReport) bool {
	for _, c := range rep.Status.Conditions {
		if c.Type == "InstallFailed" && c.Status == "True" {
			return true
		}
	}
	return false
}

// checkVersionMismatch compares NDR driverVersion with DriverInstallPolicy driver.version
func (r *DriverInstallReconciler) checkVersionMismatch(ctx context.Context, rep *npuv1alpha1.NodeDeviceReport, pols *npuv1alpha1.DriverInstallPolicyList) (bool, npuv1alpha1.DeviceEntry) {
	for _, d := range rep.Status.Devices {
		if !d.DriverLoaded || d.DriverVersion == "" {
			continue
		}
		p := matchPolicy(pols, d)
		if p == nil || p.Spec.Driver.Version == "" {
			continue
		}
		if p.Spec.Driver.Mode == "daemonset" {
			continue
		}
		if d.DriverVersion != p.Spec.Driver.Version {
			return true, d
		}
	}
	return false, npuv1alpha1.DeviceEntry{}
}

// hasDeviceWorkloads checks if any pods on the node are using device resources (nvidia.com/gpu, furiosa.ai/npu etc.)
func (r *DriverInstallReconciler) hasDeviceWorkloads(ctx context.Context, nodeName string, vendor string) (bool, error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		// Fallback: list all pods and filter
		if err := r.List(ctx, &podList); err != nil {
			return false, err
		}
	}
	for _, pod := range podList.Items {
		if pod.Spec.NodeName != nodeName {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, c := range pod.Spec.Containers {
			for resName := range c.Resources.Limits {
				rn := string(resName)
				if strings.Contains(rn, "nvidia.com/gpu") || strings.Contains(rn, "furiosa.ai/") {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// canDrainNode checks if there are other nodes that can accept the workloads
func (r *DriverInstallReconciler) canDrainNode(ctx context.Context, nodeName string) bool {
	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList); err != nil {
		return false
	}
	readyWorkers := 0
	for _, n := range nodeList.Items {
		if n.Name == nodeName {
			continue
		}
		// Skip control plane
		if _, ok := n.Labels["node-role.kubernetes.io/control-plane"]; ok {
			continue
		}
		// Check if node is ready and schedulable
		if n.Spec.Unschedulable {
			continue
		}
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				readyWorkers++
				break
			}
		}
	}
	return readyWorkers > 0
}

// cordonNode marks a node as unschedulable
func (r *DriverInstallReconciler) cordonNode(ctx context.Context, nodeName string) error {
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return err
	}
	if node.Spec.Unschedulable {
		return nil // already cordoned
	}
	base := node.DeepCopy()
	node.Spec.Unschedulable = true
	return r.Patch(ctx, &node, client.MergeFrom(base))
}

// uncordonNode marks a node as schedulable again
func (r *DriverInstallReconciler) uncordonNode(ctx context.Context, nodeName string) error {
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return err
	}
	if !node.Spec.Unschedulable {
		return nil
	}
	base := node.DeepCopy()
	node.Spec.Unschedulable = false
	return r.Patch(ctx, &node, client.MergeFrom(base))
}

// SetupWithManager wires the controller.
func (r *DriverInstallReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// NDR status.devices 변경 시에만 트리거
	pred := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldObj, ok1 := e.ObjectOld.(*npuv1alpha1.NodeDeviceReport)
			newObj, ok2 := e.ObjectNew.(*npuv1alpha1.NodeDeviceReport)
			if !ok1 || !ok2 {
				return true
			}
			return !reflect.DeepEqual(oldObj.Status.Devices, newObj.Status.Devices)
		},
	}

	// Job 이벤트를 NDR(node=name)로 리큐
	mapJobToNDR := handler.TypedEnqueueRequestsFromMapFunc[*batchv1.Job](func(ctx context.Context, j *batchv1.Job) []reconcile.Request {
		node := j.GetLabels()["node"]
		if node == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: node}}}
	})

	// 컨트롤러 빌드 (For + Typed Watch)
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&npuv1alpha1.NodeDeviceReport{}, builder.WithPredicates(pred)).
		Build(r)
	if err != nil {
		return err
	}

	// 최신 controller-runtime의 source.Kind(typed) 시그니처
	return c.Watch(source.Kind(
		mgr.GetCache(),
		&batchv1.Job{},
		mapJobToNDR,
	))
}
