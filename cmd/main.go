/*
Copyright 2025.

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

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/internal/apiserver"
	"kcloud-operator/internal/controller"
	"kcloud-operator/internal/crdapply"
	"kcloud-operator/internal/metrics"
	"kcloud-operator/internal/naming"
	"kcloud-operator/internal/operation"
	"kcloud-operator/internal/upgrade"
	"kcloud-operator/internal/verification"
	npuwebhook "kcloud-operator/internal/webhook"
	"kcloud-operator/pkg/npuctl"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(npuv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	// === apply-crds 서브명령 분기 (반드시 flag.Parse() 이전) ===
	if len(os.Args) > 1 && os.Args[1] == "apply-crds" {
		ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
		setupLog.Info("apply-crds 서브명령 시작")
		if err := crdapply.Run(context.Background()); err != nil {
			setupLog.Error(err, "apply-crds 실패")
			os.Exit(1)
		}
		setupLog.Info("apply-crds 완료")
		return
	}
	// === 기존 manager 로직 (변경 없음) ===
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var apiBindAddress, apiCertPath string
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&apiBindAddress, "api-bind-address", "",
		"Address for the management REST API (e.g. :9444). Empty (default) disables it.")
	flag.StringVar(&apiCertPath, "api-cert-path", "",
		"Directory containing the management API TLS cert (tls.crt/tls.key). Empty serves plain HTTP.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	// syncPeriod: 캐시 re-sync 주기. 60s 로 단축하여 stale informer 조기 감지.
	syncPeriod := 60 * time.Second
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "35b22ec5.ai",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
		Cache: cache.Options{SyncPeriod: &syncPeriod},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.NPUClusterPolicyReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("npuclusterpolicy-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NPUClusterPolicy")
		os.Exit(1)
	}

	if err := (&controller.DriverDaemonSetReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("driverdaemonset-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DriverDaemonSet")
		os.Exit(1)
	}

	if err := (&controller.ToolkitDaemonSetReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("toolkitdaemonset-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ToolkitDaemonSet")
		os.Exit(1)
	}

	sm := &upgrade.UpgradeStateMachine{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("driver-upgrade-statemachine"),
	}
	driverReconciler := &controller.DriverUpgradeReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		Recorder:     mgr.GetEventRecorderFor("driverupgrade-controller"),
		StateMachine: sm,
	}
	if err := driverReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DriverUpgrade")
		os.Exit(1)
	}
	// acppLiveVerifier 는 apply 검증과 근거 게이트의 allocation probe 가 공유하는 단일 인스턴스다 —
	// 따로 만들면 ACPP_PROBE_IMAGE 설정이 두 곳으로 갈라진다.
	acppLiveVerifier := controller.NewLiveVerifier(mgr.GetClient())
	acppReconciler := &controller.AcceleratorPartitionPolicyReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("acceleratorpartitionpolicy-controller"),
		Verifier: acppLiveVerifier,
		Verification: &verification.Verifier{
			Client: mgr.GetClient(),
			Prober: controller.NewAllocationProber(acppLiveVerifier),
		},
		// 삭제 경로가 위임 모드에서 노드를 되돌리기 전에 조정자와 같은 Lease 를 잡는다(같은
		// Namespace/Duration — 서로 다른 Lease 로 나뉘면 이 목적 자체가 성립하지 않는다).
		Leases: &operation.LeaseManager{
			Client: mgr.GetClient(), Namespace: naming.OperatorNamespace(),
			Holder: "kcloud-operator", Duration: 60 * time.Second,
		},
	}
	if err := acppReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AcceleratorPartitionPolicy")
		os.Exit(1)
	}
	// AcceleratorOperation 컨트롤러 — ACPP reconciler 인스턴스를 재사용한다. participant 가 그
	// seam(Verifier·Executor·Observer)을 그대로 써야 적용 로직이 갈라지지 않는다.
	if err := (&controller.AcceleratorOperationReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("accelerator-operation"),
		Participants: operation.Registry{
			operation.PartitionReconfigure: controller.NewACPPParticipant(acppReconciler),
			operation.SharingModeChange:    controller.NewACPPParticipant(acppReconciler),
			operation.Revalidate:           controller.NewRevalidateParticipant(acppReconciler),
			operation.DriverInstall:        controller.NewDriverParticipant(driverReconciler),
			operation.DriverUpgrade:        controller.NewDriverParticipant(driverReconciler),
			operation.DriverRollback:       controller.NewDriverParticipant(driverReconciler),
			operation.DevicePluginRestart:  controller.NewDevicePluginParticipant(acppReconciler),
			operation.RecoverDevice:        controller.NewRecoverDeviceParticipant(acppReconciler),
			operation.NodeReboot: controller.NewNodeRebootParticipant(
				mgr.GetClient(), os.Getenv("ACPP_MIG_JOB_IMAGE")),
		},
		Leases: &operation.LeaseManager{
			Client: mgr.GetClient(), Namespace: naming.OperatorNamespace(),
			Holder: "kcloud-operator", Duration: 60 * time.Second,
		},
		Verification: &verification.Verifier{
			Client: mgr.GetClient(),
			Prober: controller.NewAllocationProber(controller.NewLiveVerifier(mgr.GetClient())),
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AcceleratorOperation")
		os.Exit(1)
	}

	// Health 감시 컨트롤러 — 장치를 직접 바꾸지 않고 상태 기록·격리 라벨·복구 작업 생성만 한다.
	if err := (&controller.AcceleratorHealthReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("accelerator-health"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AcceleratorHealth")
		os.Exit(1)
	}
	if err := (&controller.AcceleratorWorkloadReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("acceleratorworkload-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AcceleratorWorkload")
		os.Exit(1)
	}
	// 정책과 무관한 저빈도 MIG 관측 — 정책이 없어도 보고서의 MIG 필드가 신선해야 한다.
	if err := (&controller.MigObservationReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("migobservation"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MigObservation")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// Admission webhooks(DIP/NCP validating + Pod mutating).
	//
	// webhook-cert-path 가 비면 등록 자체를 건너뛴다. 이전 주석은 "WebhookConfiguration 이
	// 없으면 무해하게 미사용으로 남는다" 고 적었으나 **틀렸다** — 핸들러를 등록하는 순간
	// controller-runtime 이 웹훅 서버를 띄우고, 인증서 경로가 비면 기본 경로
	// (/tmp/k8s-webhook-server/serving-certs)에서 찾다가 매니저가 통째로 죽는다.
	// helm 차트는 webhook.enabled=false 면 --webhook-cert-path 를 주지 않으므로
	// **차트 기본값 그대로 설치하면 operator 가 뜨지 못했다**(kind 1.28 실측 2026-08-06).
	// 라이브는 줄곧 webhook 을 켜서 운영해 와 드러나지 않았다.
	if webhookCertPath != "" {
		if err := npuwebhook.Setup(mgr); err != nil {
			setupLog.Error(err, "unable to set up admission webhooks")
			os.Exit(1)
		}
	} else {
		setupLog.Info("webhook-cert-path 가 비어 admission webhook 미등록 — 웹훅 서버도 띄우지 않는다")
	}

	// Management REST API(S5-3-②, opt-in). api-bind-address 가 비면 미기동(무영향).
	// TokenReview/SAR 는 별도 clientset, 상태·CR patch 는 매니저의 캐시 client 를 재사용한다.
	if apiBindAddress != "" {
		authnClient, err := kubernetes.NewForConfig(mgr.GetConfig())
		if err != nil {
			setupLog.Error(err, "unable to build clientset for management API auth")
			os.Exit(1)
		}
		apiSrv := &apiserver.Server{
			Addr:  apiBindAddress,
			Authn: authnClient,
			Ctl:   npuctl.NewFromClient(mgr.GetClient()),
			// 조회 엔드포인트는 매니저 캐시가 아니라 API 서버를 직접 읽는다(캐시에 Event 를
			// 태우지 않기 위해서다 — apiserver.Server.Reader 주석 참고).
			Reader: mgr.GetAPIReader(),
			Log:    ctrl.Log.WithName("apiserver"),
		}
		if len(apiCertPath) > 0 {
			apiSrv.CertFile = filepath.Join(apiCertPath, "tls.crt")
			apiSrv.KeyFile = filepath.Join(apiCertPath, "tls.key")
		}
		if err := mgr.Add(apiSrv); err != nil {
			setupLog.Error(err, "unable to add management API server to manager")
			os.Exit(1)
		}
	}

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	// reconcile-alive: 마지막 reconcile 이후 5분 초과 시 liveness 실패 처리.
	// stale informer / zombie controller 감지를 위한 추가 헬스 체크.
	if err := mgr.AddHealthzCheck("reconcile-alive", func(_ *http.Request) error {
		last := metrics.GetLastReconcileTime()
		if last.IsZero() {
			return nil // 기동 직후 grace period
		}
		if d := time.Since(last); d > 5*time.Minute {
			return fmt.Errorf("no reconcile in %v", d)
		}
		return nil
	}); err != nil {
		setupLog.Error(err, "unable to set up reconcile-alive check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
