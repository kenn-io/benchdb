package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/benchdb/internal/storage"
)

// StorageStatusReader is provided by the managed archive store.
type StorageStatusReader interface {
	StorageStatus(context.Context) (storage.StorageStatus, error)
}

type StorageStatusOutput struct {
	Body storage.StorageStatus
}

func RegisterStorage(api huma.API, reader StorageStatusReader) {
	huma.Register(api, huma.Operation{
		OperationID: "storageStatus",
		Summary:     "Storage usage and maintenance status",
		Method:      http.MethodGet,
		Path:        "/api/storage",
		Errors:      []int{http.StatusServiceUnavailable},
	}, func(ctx context.Context, _ *struct{}) (*StorageStatusOutput, error) {
		if reader == nil {
			return nil, huma.Error503ServiceUnavailable("Storage status is unavailable")
		}
		status, err := reader.StorageStatus(ctx)
		if err != nil {
			return nil, huma.Error503ServiceUnavailable("Storage status is unavailable")
		}
		return &StorageStatusOutput{Body: status}, nil
	})
}
