// ============================================================
// server_test.go: 관리 REST API 핸들러 단위 테스트
// 상세: fake clientset(TokenReview/SAR reactor) + fake controller-runtime client(npuctl)로
//       인증(401)/인가(403)/patch 매핑/오입력 거부(400)를 검증한다(라이브 불필요).
// 생성일: 2026-07-20
// ============================================================

package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	npuv1alpha1 "kcloud-operator/api/v1alpha1"
	"kcloud-operator/pkg/npuctl"
)

// authnOpts 는 fake clientset 의 TokenReview/SAR 응답을 제어한다.
type authnOpts struct {
	authenticated bool
	username      string
	allowed       bool
	// denyResources 가 지정되면 그 리소스에 대한 SAR 만 거부하고 나머지는 allowed 를 따른다
	// (복수 authz 게이트를 중첩한 라우트에서 "이 리소스만 권한 없음"을 재현하기 위함).
	denyResources map[string]bool
}

func newServer(t *testing.T, opts authnOpts, objs ...client.Object) (*Server, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	if err := npuv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("npuv1alpha1: %v", err)
	}
	crc := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&npuv1alpha1.NPUClusterPolicy{}, &npuv1alpha1.NodeDeviceReport{}, &npuv1alpha1.DriverUpgradeState{}).
		Build()

	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{
			Authenticated: opts.authenticated,
			User:          authnv1.UserInfo{Username: opts.username},
		}}, nil
	})
	cs.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		allowed := opts.allowed
		if ca, ok := action.(k8stesting.CreateAction); ok && opts.denyResources != nil {
			if sar, ok := ca.GetObject().(*authzv1.SubjectAccessReview); ok && opts.denyResources[sar.Spec.ResourceAttributes.Resource] {
				allowed = false
			}
		}
		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
	})

	return &Server{Authn: cs, Ctl: npuctl.NewFromClient(crc), Reader: crc, Log: logr.Discard()}, crc
}

func do(t *testing.T, s *Server, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, http.NoBody)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestAuth_NoToken401(t *testing.T) {
	s, _ := newServer(t, authnOpts{})
	if w := do(t, s, "GET", "/api/v1/status", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("무토큰은 401 이어야 함: got %d", w.Code)
	}
}

func TestAuth_Unauthenticated401(t *testing.T) {
	s, _ := newServer(t, authnOpts{authenticated: false})
	if w := do(t, s, "GET", "/api/v1/status", "", "bad-token"); w.Code != http.StatusUnauthorized {
		t.Fatalf("인증 실패 토큰은 401 이어야 함: got %d", w.Code)
	}
}

func TestAuth_Forbidden403(t *testing.T) {
	// 인증은 되지만 인가(SAR)에서 거부 → 403.
	s, _ := newServer(t, authnOpts{authenticated: true, username: "sa:noperm", allowed: false})
	if w := do(t, s, "GET", "/api/v1/status", "", "tok"); w.Code != http.StatusForbidden {
		t.Fatalf("무권한은 403 이어야 함: got %d", w.Code)
	}
}

func okAuth() authnOpts {
	return authnOpts{authenticated: true, username: "system:serviceaccount:kcloud:op", allowed: true}
}

func TestStatus_OK(t *testing.T) {
	ncp := &npuv1alpha1.NPUClusterPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "npuclusterpolicy-sample", Namespace: "kcloud"},
		Spec:       npuv1alpha1.NPUClusterPolicySpec{Nvidia: npuv1alpha1.NvidiaSpec{Enabled: true}},
	}
	s, _ := newServer(t, okAuth(), ncp)
	w := do(t, s, "GET", "/api/v1/status", "", "tok")
	if w.Code != http.StatusOK {
		t.Fatalf("status 200 이어야 함: got %d (%s)", w.Code, w.Body.String())
	}
	var cs npuctl.ClusterStatus
	if err := json.Unmarshal(w.Body.Bytes(), &cs); err != nil {
		t.Fatalf("응답 JSON 파싱 실패: %v", err)
	}
	if len(cs.Vendors) == 0 {
		t.Fatalf("vendors 가 비어있음")
	}
}

func TestUpgrade_PatchesDIP(t *testing.T) {
	dip := &npuv1alpha1.DriverInstallPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia-gpu-ds"},
		Spec: npuv1alpha1.DriverInstallPolicySpec{
			Vendor: "nvidia", Model: "generic",
			Driver: npuv1alpha1.DriverSpec{Version: "580.159.03"},
		},
	}
	s, crc := newServer(t, okAuth(), dip)
	w := do(t, s, "POST", "/api/v1/vendors/nvidia/upgrade", `{"version":"595.58.03"}`, "tok")
	if w.Code != http.StatusOK {
		t.Fatalf("upgrade 200 이어야 함: got %d (%s)", w.Code, w.Body.String())
	}
	var got npuv1alpha1.DriverInstallPolicy
	if err := crc.Get(context.Background(), client.ObjectKey{Name: "nvidia-gpu-ds"}, &got); err != nil {
		t.Fatalf("patch 후 DIP Get: %v", err)
	}
	if got.Spec.Driver.Version != "595.58.03" {
		t.Fatalf("DIP version patch 안됨: %s", got.Spec.Driver.Version)
	}
}

func TestToggle_PatchesNCP(t *testing.T) {
	ncp := &npuv1alpha1.NPUClusterPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "npuclusterpolicy-sample", Namespace: "kcloud"},
		Spec:       npuv1alpha1.NPUClusterPolicySpec{Nvidia: npuv1alpha1.NvidiaSpec{Enabled: true}},
	}
	s, crc := newServer(t, okAuth(), ncp)
	w := do(t, s, "POST", "/api/v1/vendors/nvidia/toggle", `{"enabled":false}`, "tok")
	if w.Code != http.StatusOK {
		t.Fatalf("toggle 200 이어야 함: got %d (%s)", w.Code, w.Body.String())
	}
	var got npuv1alpha1.NPUClusterPolicy
	if err := crc.Get(context.Background(), client.ObjectKey{Name: "npuclusterpolicy-sample", Namespace: "kcloud"}, &got); err != nil {
		t.Fatalf("patch 후 NCP Get: %v", err)
	}
	if got.Spec.Nvidia.Enabled {
		t.Fatalf("nvidia.enabled 가 false 로 patch 안됨")
	}
}

func TestUpgrade_UnknownVendor400(t *testing.T) {
	s, _ := newServer(t, okAuth())
	if w := do(t, s, "POST", "/api/v1/vendors/bogus/upgrade", `{"version":"1.0"}`, "tok"); w.Code != http.StatusBadRequest {
		t.Fatalf("알 수 없는 vendor 는 400 이어야 함: got %d", w.Code)
	}
}

func TestUpgrade_MissingVersion400(t *testing.T) {
	s, _ := newServer(t, okAuth())
	if w := do(t, s, "POST", "/api/v1/vendors/nvidia/upgrade", `{}`, "tok"); w.Code != http.StatusBadRequest {
		t.Fatalf("version 누락은 400 이어야 함: got %d", w.Code)
	}
}

func TestToggle_MissingEnabled400(t *testing.T) {
	s, _ := newServer(t, okAuth())
	if w := do(t, s, "POST", "/api/v1/vendors/nvidia/toggle", `{}`, "tok"); w.Code != http.StatusBadRequest {
		t.Fatalf("enabled 누락은 400 이어야 함: got %d", w.Code)
	}
}

// 최종 리뷰 Important #4 로 추가한 core-group nodes 게이트를 고정한다. 게이트가 있는데
// 아무 테스트도 잠그지 않으면 다음 리팩터가 조용히 지워도 초록으로 남는다 — 이 트랙이
// 반복해서 값을 치른 형태다(I-6 의 동어반복 테스트와 같은 부류, 인가 쪽 버전).
// 응답에 node 이름·schedulable·nodeAdvertised 를 싣는 5개 라우트 전부가 대상이다.
func TestNodeAuthzGateOnNodeDerivedRoutes(t *testing.T) {
	cases := []struct{ name, method, path, body string }{
		{"accelerators", "GET", "/api/v1/accelerators", ""},
		{"acceleratorByUID", "GET", "/api/v1/accelerators/GPU-aaa", ""},
		{"nodeAccelerators", "GET", "/api/v1/nodes/worker1/accelerators", ""},
		{"capabilities", "GET", "/api/v1/capabilities", ""},
		{"preview", "POST", "/api/v1/preview", `{"class":"whole-gpu","access":{"mode":"exclusive"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := authnOpts{
				authenticated: true, username: "sa:no-nodes", allowed: true,
				denyResources: map[string]bool{"nodes": true},
			}
			s, _ := newServer(t, opts, previewObjects()...)
			w := do(t, s, tc.method, tc.path, tc.body, "tok")
			if w.Code != http.StatusForbidden {
				t.Fatalf("core group nodes 권한 없으면 403 이어야 함: got %d (%s)", w.Code, w.Body.String())
			}
			// 어느 리소스가 403 에 이름을 남기는지는 authzAll 의 pairs 순서로 결정된다.
			// 순서를 고정하지 않으면 뒤집어도 스위트가 green 이라(Task 2 리뷰가 실험으로 확인)
			// 사용자가 받는 사유가 조용히 바뀐다. nodes 가 첫 게이트임을 여기서 고정한다.
			if body := w.Body.String(); !strings.Contains(body, "list nodes") {
				t.Errorf("403 은 막힌 리소스(nodes)를 이름으로 알려야 함: %s", body)
			}
		})
	}
}

// 중첩 게이트가 계층마다 TokenReview 를 반복하면 요청당 왕복이 게이트 수만큼 늘어난다.
// 토큰이 하나면 TokenReview 도 한 번이어야 한다(최종 리뷰 후속 4번).
func TestAuthzAllDoesOneTokenReview(t *testing.T) {
	s, _ := newServer(t, okAuth(), previewObjects()...)
	fake := s.Authn.(*k8sfake.Clientset)
	fake.ClearActions()
	if w := do(t, s, "POST", "/api/v1/preview", `{"class":"whole-gpu","access":{"mode":"exclusive"}}`, "tok"); w.Code != http.StatusOK {
		t.Fatalf("200 이어야 함: got %d (%s)", w.Code, w.Body.String())
	}
	trs, sars := 0, 0
	for _, a := range fake.Actions() {
		switch a.GetResource().Resource {
		case "tokenreviews":
			trs++
		case "subjectaccessreviews":
			sars++
		}
	}
	if trs != 1 {
		t.Errorf("TokenReview 는 요청당 1회여야 함: got %d", trs)
	}
	if sars != 4 {
		t.Errorf("SAR 은 게이트 수(4)만큼: got %d", sars)
	}
}

// 기존 read 라우트 2개는 npuctl.CollectStatus 를 통해 corev1.NodeList 를 읽고
// node.status.allocatable 전량을 응답에 싣는다(pkg/npuctl/types.go:68-71). npu.ai 권한만으로
// 그게 나가면 안 된다 — core group nodes list 인가를 함께 요구한다(최종 리뷰 후속 1번).
func TestStatusRoutesRequireNodeAuthz(t *testing.T) {
	cases := []struct{ name, path string }{
		{"status", "/api/v1/status"},
		{"nodeByName", "/api/v1/nodes/k8s-worker1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := authnOpts{
				authenticated: true, username: "sa:no-nodes", allowed: true,
				denyResources: map[string]bool{"nodes": true},
			}
			s, _ := newServer(t, opts)
			if w := do(t, s, "GET", tc.path, "", "tok"); w.Code != http.StatusForbidden {
				t.Fatalf("core group nodes 권한 없으면 403 이어야 함: got %d (%s)", w.Code, w.Body.String())
			}
		})
	}
}

// 두 게이트가 동시에 막혀 있을 때 403 이 이름을 남기는 리소스는 authzAll 의 pairs 순서로
// 결정된다. 순서를 고정하는 테스트가 없으면 pairs 를 뒤집어도 스위트가 green 이라(Task 2
// 리뷰가 실험으로 확인) 사용자가 받는 사유가 조용히 바뀐다. nodes 가 첫 게이트임을 고정한다.
// 거부를 하나만 걸면 순서와 무관하게 그 하나가 이름에 나오므로 반드시 둘을 막아야 한다.
func TestAuthzAllReportsFirstGateInOrder(t *testing.T) {
	opts := authnOpts{
		authenticated: true, username: "sa:no-nodes-no-acpp", allowed: true,
		denyResources: map[string]bool{"nodes": true, "acceleratorpartitionpolicies": true},
	}
	s, _ := newServer(t, opts, previewObjects()...)
	w := do(t, s, "GET", "/api/v1/accelerators", "", "tok")
	if w.Code != http.StatusForbidden {
		t.Fatalf("403 이어야 함: got %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "list nodes") {
		t.Errorf("첫 게이트(nodes)가 403 에 나와야 함: %s", body)
	}
	if strings.Contains(body, "acceleratorpartitionpolicies") {
		t.Errorf("두 번째 게이트가 먼저 보고됐다 — pairs 순서가 뒤집혔다: %s", body)
	}
}
