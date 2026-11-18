package memory_test

import (
	"testing"

	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/checkpoint/checkpointtest"
	"github.com/aswin-kevin/orchestrix/checkpoint/memory"
)

func TestConformance(t *testing.T) {
	checkpointtest.Run(t, func(*testing.T) checkpoint.Checkpointer { return memory.New() })
}
