package service

import (
	"context"
	"fmt"
)

// MachineList is the GET /api/machines response.
type MachineList struct {
	Machines []string `json:"machines" doc:"Machine names with benchmark history, sorted by name."`
}

// ListMachines returns the machines with benchmark history, within repository
// when it is not empty, so a machine filter can offer every machine rather
// than only those on a loaded page.
func (r *Reader) ListMachines(ctx context.Context, repository string) (*MachineList, error) {
	var scope *string
	if repository != "" {
		scope = &repository
	}
	machines, err := r.store.SelectBenchmarkMachines(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}
	if machines == nil {
		machines = []string{}
	}
	return &MachineList{Machines: machines}, nil
}
