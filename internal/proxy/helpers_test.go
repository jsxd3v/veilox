package proxy

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/jsxd3v/veilox/internal/detect"
	"github.com/jsxd3v/veilox/internal/mask"
)

type recorder struct {
	header http.Header
	status int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(s int)           { r.status = s }

func mustURL(s string) *url.URL {
	u, _ := url.Parse(s)
	return u
}

func newMapperFor(t *testing.T, names ...string) *mask.Mapper {
	t.Helper()
	e, err := detect.New(detect.Options{Names: names})
	if err != nil {
		t.Fatal(err)
	}
	return mask.NewMapper(e)
}
