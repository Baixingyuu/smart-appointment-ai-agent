package tooling

import (
	"errors"
	"strings"
	"testing"
)

func readTool(code string) Definition {
	return Definition{
		Code:        code,
		Description: "只读工具",
		Risk:        RiskRead,
		Parameters:  map[string]any{"type": "object"},
		Handler:     func(HandlerContext, map[string]any) (string, error) { return "ok", nil },
	}
}

func writeTool(code string) Definition {
	return Definition{
		Code:                code,
		Description:         "写工具",
		Risk:                RiskWrite,
		RequireConfirmation: true,
		Parameters:          map[string]any{"type": "object"},
		Handler:             func(HandlerContext, map[string]any) (string, error) { return "ok", nil },
	}
}

func policyWith(codes ...string) Policy {
	p := DefaultPolicy()
	p.AllowedTools = codes
	return p
}

func TestDefinitionValidateRejectsUnsafeCombinations(t *testing.T) {
	cases := []struct {
		name string
		def  Definition
	}{
		{"缺 Code", Definition{Description: "x", Risk: RiskRead, Handler: readTool("a").Handler}},
		{"缺描述", Definition{Code: "a", Risk: RiskRead, Handler: readTool("a").Handler}},
		{"缺执行函数", Definition{Code: "a", Description: "x", Risk: RiskRead}},
		{"风险等级非法", Definition{Code: "a", Description: "x", Risk: "danger", Handler: readTool("a").Handler}},
		{
			// 写操作不要求确认 == 把副作用交给模型自行决定，必须拒绝。
			"写操作未要求确认",
			Definition{Code: "a", Description: "x", Risk: RiskWrite, Handler: readTool("a").Handler},
		},
		{
			"只读操作却要求确认",
			Definition{
				Code: "a", Description: "x", Risk: RiskRead, RequireConfirmation: true,
				Handler: readTool("a").Handler,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.def.Validate(); err == nil {
				t.Fatalf("应拒绝该定义：%s", tc.name)
			}
		})
	}
}

func TestNewRegistryRejectsDuplicates(t *testing.T) {
	if _, err := NewRegistry(readTool("dup"), readTool("dup")); err == nil {
		t.Fatal("重复注册应报错")
	}
}

func TestDefinitionsAreSortedAndStable(t *testing.T) {
	// 工具列表进入 prompt 前缀，顺序漂移会破坏上游 prompt 缓存，
	// 也会让评测结果不可比对。
	registry, err := NewRegistry(readTool("z_tool"), readTool("a_tool"), readTool("m_tool"))
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	defs := registry.Definitions()
	want := []string{"a_tool", "m_tool", "z_tool"}
	for i, def := range defs {
		if def.Code != want[i] {
			t.Fatalf("工具应按编码升序，位置 %d 期望 %s 实际 %s", i, want[i], def.Code)
		}
	}
}

func TestAuthorizeRejectionsCarryStructuredKind(t *testing.T) {
	registry, err := NewRegistry(readTool("rag_search"), writeTool("ticket_create_confirm"))
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	cases := []struct {
		name string
		inv  Invocation
		want ErrorKind
	}{
		{
			"未知工具",
			Invocation{Code: "no_such_tool", Policy: policyWith("no_such_tool")},
			KindUnknownTool,
		},
		{
			"不在白名单",
			Invocation{Code: "rag_search", Policy: policyWith("other_tool")},
			KindNotAllowed,
		},
		{
			"空白名单",
			Invocation{Code: "rag_search", Policy: Policy{MaxTotalCalls: 3}},
			KindNotAllowed,
		},
		{
			"总调用超预算",
			Invocation{
				Code: "rag_search", Policy: policyWith("rag_search"),
				TotalCalls: 99,
			},
			KindBudgetExceeded,
		},
		{
			"单工具调用超预算",
			Invocation{
				Code: "rag_search", Policy: policyWith("rag_search"),
				CallCounts: map[string]int{"rag_search": 99},
			},
			KindBudgetExceeded,
		},
		{
			"参数体过大",
			Invocation{
				Code:      "rag_search",
				Policy:    policyWith("rag_search"),
				Arguments: strings.Repeat("x", 64*1024),
			},
			KindArgsTooLarge,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := registry.Authorize(tc.inv)
			if err == nil {
				t.Fatalf("应拒绝：%s", tc.name)
			}
			if got := KindOf(err); got != tc.want {
				t.Fatalf("归因应为 %s，实际 %s（%v）", tc.want, got, err)
			}
			// 归因必须可被 errors.As 提取，供上层统计。
			var toolErr *ToolError
			if !errors.As(err, &toolErr) {
				t.Fatalf("错误应可断言为 *ToolError：%v", err)
			}
		})
	}
}

func TestAuthorizeAllowsWriteToolSoConfirmFlowCanStart(t *testing.T) {
	// 回归用例：写操作的「需要确认」不是授权失败。
	//
	// 早期实现里 Authorize 直接因未确认而返回错误，导致调用方在
	// 创建确认中断之前就短路，写操作永远无法发起。
	// 授权只判断「能不能调用」，流程位置由编排层负责。
	registry, err := NewRegistry(writeTool("ticket_create_confirm"))
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	def, err := registry.Authorize(Invocation{
		Code: "ticket_create_confirm", Policy: policyWith("ticket_create_confirm"),
	})
	if err != nil {
		t.Fatalf("写工具在授权阶段应放行，以便落确认中断，实际报错: %v", err)
	}
	if !def.RequireConfirmation {
		t.Error("放行不等于免确认：定义上必须仍标记需要确认")
	}
	if !registry.ConfirmationRequired("ticket_create_confirm") {
		t.Error("ConfirmationRequired 应返回 true，编排层据此落中断")
	}
}

func TestAuthorizeZeroPolicyFallsBackToDefaults(t *testing.T) {
	// 零值策略必须回落到默认值而不是「预算为 0 全部拒绝」，
	// 否则忘记配置策略会让所有工具静默失效。
	registry, err := NewRegistry(readTool("rag_search"))
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	policy := Policy{AllowedTools: []string{"rag_search"}}
	if _, err := registry.Authorize(Invocation{Code: "rag_search", Policy: policy}); err != nil {
		t.Fatalf("零值预算应回落默认值，实际报错: %v", err)
	}
	if policy.MaxTotalCalls != 0 {
		t.Error("normalize 不应就地修改调用方传入的策略（避免隐藏副作用）")
	}
}

func TestParseArguments(t *testing.T) {
	def := readTool("rag_search")
	def.Required = []string{"query"}

	t.Run("合法参数", func(t *testing.T) {
		args, err := ParseArguments(def, `{"query":"接口报错"}`)
		if err != nil {
			t.Fatalf("应解析成功: %v", err)
		}
		if StringArg(args, "query") != "接口报错" {
			t.Fatalf("参数值不符：%v", args)
		}
	})

	t.Run("空参数视为空对象", func(t *testing.T) {
		args, err := ParseArguments(readTool("no_args"), "")
		if err != nil {
			t.Fatalf("无参工具允许空字符串: %v", err)
		}
		if len(args) != 0 {
			t.Fatalf("期望空对象，实际 %v", args)
		}
	})

	t.Run("非法 JSON", func(t *testing.T) {
		_, err := ParseArguments(def, `{not json`)
		if KindOf(err) != KindInvalidArgs {
			t.Fatalf("应归因为 invalid_args，实际 %s", KindOf(err))
		}
	})

	t.Run("缺少必填项", func(t *testing.T) {
		_, err := ParseArguments(def, `{}`)
		if KindOf(err) != KindInvalidArgs {
			t.Fatalf("应归因为 invalid_args，实际 %s", KindOf(err))
		}
	})

	t.Run("必填项为空字符串", func(t *testing.T) {
		// 显式区分「未提供」与「提供了空值」：后者多半是模型没想清楚
		// 就发起调用，直接拒绝比让业务层收到空值更好。
		_, err := ParseArguments(def, `{"query":"   "}`)
		if KindOf(err) != KindInvalidArgs {
			t.Fatalf("空字符串必填项应归因为 invalid_args，实际 %s", KindOf(err))
		}
	})
}

func TestArgReadersTolerateWrongTypes(t *testing.T) {
	// 模型可能给出类型不符的参数。读取器必须宽容返回零值，
	// 而不是 panic —— 一次参数类型错误不该让整个回合崩溃。
	args := map[string]any{
		"count": 42,
		"list":  "not-a-list",
		"ok":    "text",
		"items": []any{"a", "", "  b  ", 3},
	}
	if got := StringArg(args, "count"); got != "" {
		t.Errorf("类型不符应返回空字符串，实际 %q", got)
	}
	if got := StringSliceArg(args, "list"); got != nil {
		t.Errorf("类型不符应返回 nil，实际 %v", got)
	}
	if got := StringArg(args, "missing"); got != "" {
		t.Errorf("缺失键应返回空字符串，实际 %q", got)
	}
	// 数组内应过滤空字符串、去空白，并忽略非字符串项。
	items := StringSliceArg(args, "items")
	if len(items) != 2 || items[0] != "a" || items[1] != "b" {
		t.Errorf("数组读取应过滤空值并去空白，实际 %v", items)
	}
}

func TestKindOfDefaultsToExecFailed(t *testing.T) {
	// 非工具错误必须归为 exec_failed，否则统计会把业务异常
	// 混进「越权尝试」之类的治理指标里。
	if got := KindOf(errors.New("普通错误")); got != KindExecFailed {
		t.Fatalf("普通错误应归为 exec_failed，实际 %s", got)
	}
	if got := KindOf(nil); got != KindExecFailed {
		t.Fatalf("nil 亦应回落 exec_failed，实际 %s", got)
	}
}
