// v2 派单评测集的加载与回归闸门。
//
// 本测试证明什么、不证明什么，必须先说清楚：
// v2 的金标由 eval/datasets/gen_v2.py 复现 Stage 1 规则派生，
// 所以"三段轴全过"只等于 **pipeline 实现 == 我们宣称的规则**，
// 不构成对外准确率声明（那需要人工标注卡，见数据集的 goldBoundary 字段）。
//
// 因此这里刻意把力气放在派生过程 **抓不到** 的四类漂移上：
//  1. 金标引用的服务/员工 ID 在 seed 里是否还存在（改花名册最常见的静默破坏）；
//  2. 金标三段之间是否自相矛盾（outcome 与 assigneeId 的取值约定）；
//  3. 出处元数据是否被删（provenance/goldSource/goldBoundary 是对外声明的边界）；
//  4. 金标分布是否塌成单一结果（全同一 winner 的数据集没有区分力）。
//
// 取代了原先的 eval/verify_datasets.py：那份独立校验器服务于已删除的 v1
// 技能数据集（用精确有理数重算 Jaccard 加权和）。v2 的主要失败模式是与
// seed 目录漂移，而 seed 只有 Go 侧能读到，Python 重算反而要多养一份花名册。
package eval

import (
	"path/filepath"
	"testing"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/seed"
)

func TestLoadPipelineV2Dataset(t *testing.T) {
	ds := loadRealV2Dataset(t)

	if ds.Version != 2 || !ds.PipelineAxes {
		t.Errorf("数据集标记不匹配：version=%d pipelineAxes=%v", ds.Version, ds.PipelineAxes)
	}
	if len(ds.Cases) == 0 {
		t.Fatal("数据集为空")
	}
	// 出处与金标边界是对外声明的一部分，删掉就等于把"派生金标"说成"人工金标"。
	provenance := map[string]PipelineV2ProvenanceEntry{
		"primary":     ds.Provenance.Primary,
		"sanityCheck": ds.Provenance.SanityCheck,
	}
	for name, entry := range provenance {
		if entry.Name == "" || entry.URL == "" || entry.License == "" {
			t.Errorf("provenance.%s 缺字段（name/url/license 都要可核验）", name)
		}
	}
	if ds.GoldSource == "" {
		t.Error("goldSource 缺失：金标来源必须写明")
	}
	if len([]rune(ds.GoldBoundary)) < 40 {
		t.Errorf("goldBoundary 过短，已失去约束力：%q", ds.GoldBoundary)
	}
}

// TestPipelineV2GoldConsistentWithSeed 校验金标引用的 ID 与规则自洽性。
func TestPipelineV2GoldConsistentWithSeed(t *testing.T) {
	ds := loadRealV2Dataset(t)

	services := seed.ServicesByID()
	extensions := map[int64]domain.EmployeeExtension{}
	for _, ext := range seed.EmployeeExtensions() {
		extensions[ext.EmployeeID] = ext
	}

	seen := map[string]struct{}{}
	winners := map[int64]int{}
	for _, item := range ds.Cases {
		if item.ID == "" {
			t.Fatal("存在空 case id")
		}
		if _, dup := seen[item.ID]; dup {
			t.Errorf("case id 重复：%s", item.ID)
		}
		seen[item.ID] = struct{}{}

		if item.Ticket.Title == "" || item.Ticket.Category == "" || item.Ticket.Priority == "" {
			t.Errorf("%s：工单原文不完整", item.ID)
		}
		if item.Expected.ServiceTop1 != 0 {
			if _, ok := services[item.Expected.ServiceTop1]; !ok {
				t.Errorf("%s：金标服务 %d 不在 seed 服务字典", item.ID, item.Expected.ServiceTop1)
			}
		}

		switch item.Expected.Outcome {
		case string(domain.OutcomeMatched):
			if _, ok := extensions[item.Expected.AssigneeID]; !ok {
				// 改花名册忘了重生成金标时，评测会静默地把不存在的员工当成错答案，
				// 归因就跑偏了，所以这里必须硬失败。
				t.Errorf("%s：金标员工 %d 不在 seed 花名册", item.ID, item.Expected.AssigneeID)
			}
			if item.Expected.Weakness != assign.WeaknessNone.String() {
				// 数据集里写的是 String() 形式："none"，不是 Go 零值空串。
				t.Errorf("%s：matched 样本的 weakness 应为 none，实际 %q", item.ID, item.Expected.Weakness)
			}
			winners[item.Expected.AssigneeID]++
		case string(domain.OutcomeFallbackPool):
			if item.Expected.AssigneeID != 0 {
				t.Errorf("%s：fallback_pool 样本必须无人被指派，实际 %d", item.ID, item.Expected.AssigneeID)
			}
			// 判弱必须是离散原因：没有归因的"退回人工池"无法用于定位是哪一段失效。
			if item.Expected.Weakness == "" || item.Expected.Weakness == assign.WeaknessNone.String() {
				t.Errorf("%s：fallback_pool 样本缺 weakness 归因", item.ID)
			}
		default:
			t.Errorf("%s：未知 outcome %q", item.ID, item.Expected.Outcome)
		}
	}

	// 区分力自检：matched 样本若全落到同一个人，排序轴实际上什么都没测
	// （固定答那个人就能拿满分）。
	if len(winners) < 3 {
		t.Errorf("matched 样本的 winner 只有 %d 个不同员工，排序轴缺乏区分力：%v", len(winners), winners)
	}
}

// TestPipelineV2OfflineAxesRegressionFloor 是三段轴的回归地板。
//
// 走 Stage 1 + Stage 3（不注入 chooser）。
//
// 为什么断言的是地板而不是 100%：2026-09-24 把 ownershipScore 从 1.0/0.6/0.3
// 重标定为 1.0/0.3/0.1（docs/DISPATCH_PIPELINE.md §1.4），数据集金标是重标定之前
// 由 gen_v2.py 派生的，没有跟着重生成。当前实测读数 抽取 42/45、判弱 38/45、
// 排序 34/40、Stage 1 直出 35，与文档记录一致。
// 剩余失败两类原因：BM25 多召回第二条过阈值服务（ownership-leak）与真实抽取歧义。
//
// 地板的作用是把"再退化"变成构建失败；把它当"当前准确率"用是误读，
// 那需要重生成金标 + 人工标注卡。
func TestPipelineV2OfflineAxesRegressionFloor(t *testing.T) {
	ds := loadRealV2Dataset(t)

	pipeline := assign.NewPipeline(
		assign.NewBM25ServiceResolver(3),
		assign.NoopSimilarIndex{},
		nil,
	)
	rep := RunPipelineV2Eval(ds, pipeline, seedV2Directory())

	checks := []struct {
		name string
		want PipelineV2Axis
		got  PipelineV2Axis
	}{
		{"抽取", PipelineV2Axis{Applicable: 45, Pass: 42}, rep.Extraction},
		{"判弱", PipelineV2Axis{Applicable: 45, Pass: 38}, rep.Weakness},
		{"排序", PipelineV2Axis{Applicable: 40, Pass: 34}, rep.Sort},
	}
	for _, c := range checks {
		if c.got.Applicable != c.want.Applicable {
			t.Errorf("%s 轴可判样本数变了：期望 %d，实际 %d（数据集或轴定义被改动）",
				c.name, c.want.Applicable, c.got.Applicable)
		}
		if c.got.Pass < c.want.Pass {
			t.Errorf("%s 轴退化：%d/%d，地板是 %d/%d",
				c.name, c.got.Pass, c.got.Applicable, c.want.Pass, c.want.Applicable)
		}
	}
	if rep.Funnel.Stage1Direct < 35 {
		t.Errorf("Stage 1 直出数退化：%d，地板是 35（总 %d）", rep.Funnel.Stage1Direct, rep.Total)
	}
	// Stage 2 未注入，漏斗里出现 stage2_* 说明 pipeline 把样本交给了不存在的 chooser。
	if rep.Funnel.Stage2 != 0 {
		t.Errorf("离线模式下 stage2 路径应为 0，实际 %d", rep.Funnel.Stage2)
	}
	if rep.Funnel.Stage1Direct+rep.Funnel.Stage3 != rep.Total {
		t.Errorf("漏斗不闭合：stage1=%d stage3=%d total=%d",
			rep.Funnel.Stage1Direct, rep.Funnel.Stage3, rep.Total)
	}
	if len(rep.Failures) > 0 {
		t.Logf("已知未命中金标 %d 条（重标定后未重生成金标，非回归）", len(rep.Failures))
	}
}

// seedV2Directory 与 eval-assign-v2 命令同源：负载全部归零。
// 起点不一致会让同一条 case 两次跑落到不同员工，指标就不可复现。
func seedV2Directory() assign.Directory {
	emps := seed.Employees()
	for i := range emps {
		emps[i].CurrentLoad = 0
	}
	return assign.Directory{
		Employees:  emps,
		Extensions: seed.ExtensionsByEmployeeID(),
		Services:   seed.ServicesByID(),
	}
}

func loadRealV2Dataset(t *testing.T) *PipelineV2Dataset {
	t.Helper()
	path := filepath.Join("..", "..", "eval", "datasets", "assignment_v2.json")
	ds, err := LoadPipelineV2Dataset(path)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}
