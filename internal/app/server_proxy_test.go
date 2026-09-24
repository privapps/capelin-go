package app

import (
	"context"
	"net/http"

	"capelin-go/internal/server"
)

func newComposedProxyHandler(origin string, client *http.Client) http.Handler {
	if _, err := parseAbsoluteTarget(origin); err != nil {
		panic(err)
	}
	a := &app{
		cfg: config{
			securityEnabled: true,
			securityPolicy:  serverSecurityPolicy{AllowPrivateTargets: true},
		},
		dataStore: newDataStore(),
		proxyHTTP: client,
	}
	return newServerHandlerWithExecutor(a, nil, server.ExecutorFunc(func(_ context.Context, _ *server.ExecutionRequest) (server.ExecutionResult, error) {
		return server.ExecutionResult{}, nil
	}))
}
