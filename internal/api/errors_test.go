package api

import (
	"net/http"
	"testing"

	"github.com/dengyie/panrouter/internal/driver"
)

func TestKindStatusMapping(t *testing.T) {
	cases := map[driver.Kind]int{
		driver.KindNotFound:         http.StatusNotFound,
		driver.KindShareGone:        http.StatusNotFound,
		driver.KindAuthExpired:      http.StatusUnauthorized,
		driver.KindSessionExpired:   http.StatusUnauthorized,
		driver.KindAuthInvalid:      http.StatusUnauthorized,
		driver.KindRiskControl:      http.StatusTooManyRequests,
		driver.KindUnsupported:      http.StatusBadRequest,
		driver.KindInterfaceChanged: http.StatusBadGateway,
		driver.KindUpstream:         http.StatusBadGateway,
	}
	for k, want := range cases {
		if got := kindStatus(k); got != want {
			t.Errorf("kindStatus(%s)=%d want %d", k, got, want)
		}
	}
}
