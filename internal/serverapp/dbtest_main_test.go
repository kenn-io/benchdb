package serverapp

import (
	"testing"

	"go.kenn.io/benchdb/internal/dbtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }
