package app

import (
	"context"
	"net/http"

	"capelin-go/internal/server"
)

func newComposedProxyHandler(origin string, client *http.Client) http.Handler {
	parsed, err := parseAbsoluteTarget(origin)
	if err != nil {
		panic(err)
	}
	a := &app{
		cfg: config{
			securityEnabled: true,
			securityPolicy: serverSecurityPolicy{
				AllowedTargets:      map[string]bool{parsed.Origin: true},
				AllowPrivateTargets: true,
			},
		},
		dataStore: newDataStore(),
		proxyHTTP: client,
	}
	return newServerHandlerWithExecutor(a, nil, server.ExecutorFunc(func(_ context.Context, _ *server.ExecutionRequest) (server.ExecutionResult, error) {
		return server.ExecutionResult{}, nil
	}))
}
