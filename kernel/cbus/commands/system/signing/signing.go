package signing

import (
	"context"

	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func Status(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		if serviceSet.Signing == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "signing key unavailable")
		}
		return core.Result{"fingerprint": serviceSet.Signing.Fingerprint()}, nil
	}
}

func Replace(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, request core.Request) (core.Result, error) {
		if serviceSet.Signing == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "signing key unavailable")
		}
		key, _ := request.Arguments["key"].(string)
		if err := serviceSet.Signing.Replace(key); err != nil {
			return nil, core.NewError(core.CodeInvalidArguments, err.Error())
		}
		return core.Result{"fingerprint": serviceSet.Signing.Fingerprint()}, nil
	}
}
