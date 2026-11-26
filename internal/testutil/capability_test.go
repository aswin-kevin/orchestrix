package testutil_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

type plain struct{ tool.Tool }

type capable struct {
	tool.Tool
}

func (capable) ReadOnly(json.RawMessage) bool { return true }
func (capable) CheckPermission(context.Context, json.RawMessage, permission.Context) permission.Decision {
	return permission.Decision{Message: "checked"}
}

func TestCapabilityHelpers(t *testing.T) {
	ctx := t.Context()
	if testutil.IsReadOnly(plain{}, nil) || testutil.SelfCheck(ctx, plain{}, nil, permission.Context{}).Message != "" {
		t.Error("a tool without the capabilities must give the zero answers")
	}
	if !testutil.IsReadOnly(capable{}, nil) || testutil.SelfCheck(ctx, capable{}, nil, permission.Context{}).Message != "checked" {
		t.Error("a tool with the capabilities must be asked")
	}
}
