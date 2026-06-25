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
	cs.PrependReactor("create", "subjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: opts.allowed}}, nil
	})

	return &Server{Authn: cs, Ctl: npuctl.NewFromClient(crc), Log: logr.Discard()}, crc
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
