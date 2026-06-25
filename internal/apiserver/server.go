// ============================================================
// server.go: 관리 REST API 서버 (S5-3-②, 관리 API 2단계)
// 상세: operator pod 내 별도 HTTPS listener(:9444). pkg/npuctl 를 감싸 read(GET /status,
//       /nodes/{node})/control(POST /vendors/{vendor}/upgrade|rollback|toggle)을 노출.
//       control 은 전부 CR patch 로 귀결(1단계 계약 유지). 인증=TokenReview, 인가=SubjectAccessReview.
// 생성일: 2026-07-20
// ============================================================

package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"kcloud-operator/pkg/npuctl"
)

// apiGroup: SubjectAccessReview 인가에 사용하는 리소스 그룹(CRD group).
const apiGroup = "npu.ai"

// knownVendors: control 요청에서 허용하는 vendor 집합(npuctl 상수와 1:1).
var knownVendors = map[string]bool{
	npuctl.VendorNvidia:      true,
	npuctl.VendorFuriosa:     true,
	npuctl.VendorRngd:        true,
	npuctl.VendorRebellions:  true,
	npuctl.VendorTenstorrent: true,
}

// Server 는 관리 REST API 를 서빙합니다. Authn 은 TokenReview/SAR 용 clientset,
// Ctl 은 상태 집약·CR patch 를 수행하는 npuctl.Client 입니다.
type Server struct {
	Addr     string // 예: ":9444"
	CertFile string // TLS 인증서(파일). 비면 평문 HTTP(테스트/개발 전용).
	KeyFile  string // TLS 키(파일).
	Authn    kubernetes.Interface
	Ctl      *npuctl.Client
	Log      logr.Logger
}

// NeedLeaderElection 은 false 를 반환하여 API 서버가 리더 여부와 무관하게 서빙되게 합니다
// (읽기·CR patch 는 어느 replica 에서 수행해도 동일 계약).
func (s *Server) NeedLeaderElection() bool { return false }

// Start 는 manager.Runnable 진입점입니다. ctx 취소 시 graceful shutdown 합니다.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if s.CertFile != "" {
			s.Log.Info("management API server listening (HTTPS)", "addr", s.Addr)
			errCh <- srv.ListenAndServeTLS(s.CertFile, s.KeyFile)
			return
		}
		s.Log.Info("management API server listening (HTTP, no cert)", "addr", s.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Handler 는 라우팅된 http.Handler 를 반환합니다(TLS 없이 테스트에서 직접 소비 가능).
// Go 1.22+ 메서드/와일드카드 패턴 라우팅 사용.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// read: npu.ai 리소스 list 권한 요구.
	mux.HandleFunc("GET /api/v1/status", s.authz("list", "driverinstallpolicies", s.handleStatus))
	mux.HandleFunc("GET /api/v1/nodes/{node}", s.authz("list", "driverinstallpolicies", s.handleNode))
	// control: upgrade/rollback 은 DIP patch, toggle 은 NCP patch 권한 요구.
	mux.HandleFunc("POST /api/v1/vendors/{vendor}/upgrade", s.authz("patch", "driverinstallpolicies", s.handleUpgrade))
	mux.HandleFunc("POST /api/v1/vendors/{vendor}/rollback", s.authz("patch", "driverinstallpolicies", s.handleUpgrade))
	mux.HandleFunc("POST /api/v1/vendors/{vendor}/toggle", s.authz("patch", "npuclusterpolicies", s.handleToggle))
	return mux
}

// ctxUserKey: 인증된 주체(username)를 request context 로 전달하는 키.
type ctxUserKey struct{}

// authz 는 Bearer 토큰을 TokenReview 로 인증하고 SubjectAccessReview 로 인가한 뒤 next 를 호출합니다.
// 무토큰 401 / 인증 실패 401 / 무권한 403 / 리뷰 API 오류 500.
func (s *Server) authz(verb, resource string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}

		tr, err := s.Authn.AuthenticationV1().TokenReviews().Create(r.Context(),
			&authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token}}, metav1.CreateOptions{})
		if err != nil {
			s.Log.Error(err, "TokenReview 실패")
			writeError(w, http.StatusInternalServerError, "token review failed")
			return
		}
		if !tr.Status.Authenticated {
			writeError(w, http.StatusUnauthorized, "token not authenticated")
			return
		}
		user := tr.Status.User

		sar, err := s.Authn.AuthorizationV1().SubjectAccessReviews().Create(r.Context(),
			&authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
				User:   user.Username,
				UID:    user.UID,
				Groups: user.Groups,
				ResourceAttributes: &authzv1.ResourceAttributes{
					Group: apiGroup, Resource: resource, Verb: verb,
				},
			}}, metav1.CreateOptions{})
		if err != nil {
			s.Log.Error(err, "SubjectAccessReview 실패")
			writeError(w, http.StatusInternalServerError, "authorization review failed")
			return
		}
		if !sar.Status.Allowed {
			writeError(w, http.StatusForbidden,
				fmt.Sprintf("user %q not allowed to %s %s.%s", user.Username, verb, resource, apiGroup))
			return
		}

		next(w, r.WithContext(context.WithValue(r.Context(), ctxUserKey{}, user.Username)))
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.Ctl.CollectStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("node")
	status, err := s.Ctl.CollectStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i := range status.Nodes {
		if status.Nodes[i].Name == name {
			writeJSON(w, http.StatusOK, status.Nodes[i])
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
}

// controlRequest 는 upgrade/rollback/toggle 요청 본문입니다.
type controlRequest struct {
	Version string `json:"version,omitempty"`
	Model   string `json:"model,omitempty"`
	Auto    bool   `json:"auto,omitempty"`
	Force   bool   `json:"force,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

// handleUpgrade 는 upgrade/rollback 을 처리합니다(둘 다 DIP.spec.driver.version patch 로 귀결).
func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	vendor := r.PathValue("vendor")
	if !knownVendors[vendor] {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown vendor %q", vendor))
		return
	}
	req, ok := decodeBody(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Version) == "" {
		writeError(w, http.StatusBadRequest, "version is required")
		return
	}

	result, err := s.Ctl.UpgradeVendor(r.Context(), vendor, req.Version,
		npuctl.UpgradeOptions{Model: req.Model, Auto: req.Auto, Force: req.Force})
	if err != nil {
		// UpgradeVendor 오류는 대부분 입력 오류(미매칭 vendor/model, 빈 version)이므로 400.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// action = 마지막 경로 세그먼트("upgrade" | "rollback").
	s.audit(r, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:],
		vendor, fmt.Sprintf("version=%s->%s noChange=%t", result.PreviousVersion, result.NewVersion, result.NoChange))
	writeJSON(w, http.StatusOK, result)
}

// handleToggle 은 NPUClusterPolicy.spec.<vendor>.enabled 를 patch 합니다.
func (s *Server) handleToggle(w http.ResponseWriter, r *http.Request) {
	vendor := r.PathValue("vendor")
	if !knownVendors[vendor] {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown vendor %q", vendor))
		return
	}
	req, ok := decodeBody(w, r)
	if !ok {
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled (bool) is required")
		return
	}

	result, err := s.Ctl.SetVendorEnabled(r.Context(), vendor, *req.Enabled)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "toggle", vendor,
		fmt.Sprintf("enabled=%t->%t noChange=%t", result.PreviousEnabled, result.NewEnabled, result.NoChange))
	writeJSON(w, http.StatusOK, result)
}

// audit 은 모든 control 요청을 구조화 로그(주체·행위·대상)로 남깁니다.
func (s *Server) audit(r *http.Request, action, target, detail string) {
	subject, _ := r.Context().Value(ctxUserKey{}).(string)
	s.Log.Info("management API audit",
		"subject", subject, "action", action, "target", target, "detail", detail,
		"remoteAddr", r.RemoteAddr)
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// decodeBody 는 본문을 controlRequest 로 파싱합니다. 빈 본문(EOF)은 허용하고
// (필드 검증에서 재차 거름), 그 외 파싱 오류는 400 을 씁니다.
func decodeBody(w http.ResponseWriter, r *http.Request) (controlRequest, bool) {
	var req controlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return req, false
	}
	return req, true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
