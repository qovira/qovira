package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/qovira/qovira/internal/buildinfo"
)

// HealthOutput is the Huma output struct for GET /api/v1/health. The Body field follows Huma's convention for
// typed response bodies — it is serialized directly as the JSON response object.
type HealthOutput struct {
	Body struct {
		// Status is always "ok" for a healthy instance.
		Status string `json:"status" example:"ok"`

		// Version is the semantic release tag (e.g. "v0.1.0") or "(devel)" for unstamped builds.
		Version string `json:"version" example:"v0.1.0"`

		// Commit is the short VCS revision that produced this binary, optionally suffixed with "-dirty".
		Commit string `json:"commit" example:"f755bf8"`

		// BuildTime is the RFC 3339 timestamp of when the binary was compiled. Empty for unstamped dev builds.
		BuildTime string `json:"buildTime" format:"date-time"`
	}
}

// registerMeta registers the meta operations on the given Huma API. Currently only the health endpoint is
// registered here; this is the template every subsequent huma.Register call in this package copies.
func registerMeta(ha huma.API, bi buildinfo.Info) {
	huma.Register(ha, withMaxBodyBytes(huma.Operation{
		OperationID: "get-health",
		Method:      http.MethodGet,
		Path:        "/health",
		Summary:     "Health & build identity",
		Description: "Returns 200 when the server is accepting requests, along with the build identity (version, commit, buildTime).",
		Tags:        []string{"meta"},
	}), func(_ context.Context, _ *struct{}) (*HealthOutput, error) {
		out := &HealthOutput{}
		out.Body.Status = "ok"
		out.Body.Version = bi.Version
		out.Body.Commit = bi.Commit
		out.Body.BuildTime = bi.BuildTime

		return out, nil
	})
}
