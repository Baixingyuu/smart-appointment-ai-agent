package eval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mac/agentdesk/internal/assign"
	"github.com/mac/agentdesk/internal/domain"
)

// datasetPath 返回基础评测集路径。
func datasetPath(t *testing.T) string {
	t.Helper()
	return findDataset(t, "assignment.json")
}

// hardDatasetPath 返回对抗样本集路径。
func hardDatasetPath(t *testing.T) string {
	t.Helper()
	return findDataset(t, "assignment_hard.json")
}

func findDataset(t *testing.T, name string) string {
	t.Helper()
	candidates := []string{
		filepath.Join("..", "..", "eval", "datasets", name),
		filepath.Join("eval", "datasets", name),
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	t.Skipf("未找到 %s，已尝试 %v", name, candidates)
	return ""
}

func loadDataset(t *testing.T) *AssignmentDataset {
	t.Helper()
	dataset, err := LoadAssignmentDataset(datasetPath(t))
	if err != nil {
		t.Fatalf("加载基础评测集失败: %v", err)
	}
	return dataset
}

func loadHardDataset(t *testing.T) *AssignmentDataset {
	t.Helper()
	dataset, err := LoadAssignmentDataset(hardDatasetPath(t))
	if err != nil {
		t.Fatalf("加载对抗评测集失败: %v", err)
	}
	return dataset
}

func TestDatasetStructure(t *testing.T) {
	dataset := loadDataset(t)

	if len(dataset.Cases) != 120 {
		t.Fatalf("期望 120 条样本，实际 %d", len(dataset.Cases))
	}

	seen := make(map[string]bool, len(dataset.Cases))
	noMatchCount := 0
	scenarios := make(map[string]int)
	for _, item := range dataset.Cases {
		if item.ID == "" {
			t.Fatal("存在缺失 id 的样本")
		}
		if seen[item.ID] {
			t.Fatalf("样本 id 重复: %s", item.ID)
		}
		seen[item.ID] = true

		if len(item.Employees) < 2 {
			t.Fatalf("%s: 候选员工应至少 2 人，实际 %d", item.ID, len(item.Employees))
		}
		// 员工 ID 在样本内必须唯一，否则金标不可判定。
		empIDs := make(map[int64]bool, len(item.Employees))
		for _, emp := range item.Employees {
			if empIDs[emp.ID] {
				t.Fatalf("%s: 员工 id 重复 %d", item.ID, emp.ID)
			}
			empIDs[emp.ID] = true
		}
		if item.Expect.NoMatch {
			noMatchCount++
			if item.Expect.GoldAssigneeID != 0 {
				t.Fatalf("%s: noMatch 样本不应有金标处理人，实际 %d", item.ID, item.Expect.GoldAssigneeID)
			}
			continue
		}
		// 非 noMatch 的金标必须是该样本内的真实员工，
		// 否则金标无法由候选集产生，评测必然失败。
		if !empIDs[item.Expect.GoldAssigneeID] {
			t.Fatalf("%s: 金标处理人 %d 不在候选员工中", item.ID, item.Expect.GoldAssigneeID)
		}
		scenarios[item.Scenario]++
	}

	if noMatchCount != 10 {
		t.Errorf("期望 10 条 noMatch 样本，实际 %d", noMatchCount)
	}
	wantScenarios := map[string]int{
		"specialist_available": 30,
		"specialist_busy":      25,
		"tie_resolved_by_load": 20,
		"recency_breaks_tie":   15,
		"partial_overlap":      10,
	}
	for name, want := range wantScenarios {
		if got := scenarios[name]; got != want {
			t.Errorf("场景 %s: 期望 %d 条，实际 %d", name, want, got)
		}
	}
}

// TestHardDatasetStructure 校验对抗集结构。
func TestHardDatasetStructure(t *testing.T) {
	dataset := loadHardDataset(t)
	if len(dataset.Cases) != 30 {
		t.Fatalf("期望 30 条对抗样本，实际 %d", len(dataset.Cases))
	}

	// 对抗集必须真的「对抗」：每条样本都要有一个技能与需求无交集的竞争者。
	// 否则它无法压制「忽略技能」这一回归，也就失去了存在意义。
	adversarial := 0
	for _, item := range dataset.Cases {
		required := domain.NewSkillSet(item.Ticket.RequiredSkillIDs...)
		for _, emp := range item.Employees {
			if emp.ID == item.Expect.GoldAssigneeID {
				continue
			}
			if required.Jaccard(domain.NewSkillSet(emp.SkillIDs...)) == 0 {
				adversarial++
				break
			}
		}
	}
	if adversarial < 25 {
		t.Fatalf("对抗集区分力不足：仅 %d/%d 条含零技能竞争者，期望 ≥25",
			adversarial, len(dataset.Cases))
	}
}

// TestBaselinePassesAllCases 断言基线实现与两份评测集的规格一致。
//
// ⚠️ 解读警告：两份数据集的金标都由派单器的评分规则推导而来，
// 因此 100% 是与规格一致性的检查，**不是真实场景的准确率**。
// 真实准确率需要「工单 → 人工实际指派」的历史数据（见 PHASE_ROADMAP 三期）。
// 本用例的价值在于：规格或实现任一被误改时立即失败。
func TestBaselinePassesAllCases(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dataset *AssignmentDataset
	}{
		{"基础集", loadDataset(t)},
		{"对抗集", loadHardDataset(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := RunAssignmentEval(tc.dataset, assign.New(assign.DefaultWeights()))
			if report.Passed != report.Total {
				t.Fatalf("基线应通过全部样本，实际 %d/%d；首批失败：%+v",
					report.Passed, report.Total, firstFailures(report, 3))
			}
			if report.Top1Accuracy != 1.0 {
				t.Fatalf("期望 Top-1 准确率 1.0，实际 %f", report.Top1Accuracy)
			}
		})
	}
}

// TestBaselineOutcomeDistribution 锁定基础集的结果分布。
//
// 分布比通过率更能暴露行为变化：即便通过率仍是 100%，
// 若 fallback_pool 数量改变，说明兜底逻辑被改动过。
func TestBaselineOutcomeDistribution(t *testing.T) {
	report := RunAssignmentEval(loadDataset(t), assign.New(assign.DefaultWeights()))

	counts := map[string]int{}
	for _, result := range report.Results {
		counts[result.Outcome]++
	}

	want := map[string]int{
		string(domain.OutcomeMatched):      100,
		string(domain.OutcomeFallbackPool): 10, // 无技能交集，退回待认领池但给出建议人选
		string(domain.OutcomeNoCandidate):  10, // 全部离线或达并发上限
	}
	for outcome, wantCount := range want {
		if counts[outcome] != wantCount {
			t.Errorf("outcome %s: 期望 %d 条，实际 %d", outcome, wantCount, counts[outcome])
		}
	}
}

// TestNeverAssignsUnavailableEmployee 直接断言「过滤先于评分」这一不变量。
//
// 不依赖数据集设计：遍历全部样本，任何不在职或已达并发上限的员工
// 都不得成为最终被指派的人。这是兜底性检查，比依赖某个场景更可靠。
func TestNeverAssignsUnavailableEmployee(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dataset *AssignmentDataset
	}{
		{"基础集", loadDataset(t)},
		{"对抗集", loadHardDataset(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := RunAssignmentEval(tc.dataset, assign.New(assign.DefaultWeights()))

			assignedByCase := make(map[string]int64, len(report.Results))
			for _, result := range report.Results {
				assignedByCase[result.CaseID] = result.GotID
			}

			for _, item := range tc.dataset.Cases {
				gotID := assignedByCase[item.ID]
				if gotID == 0 {
					continue
				}
				for _, emp := range item.Employees {
					if emp.ID != gotID {
						continue
					}
					if !emp.Active {
						t.Errorf("%s: 指派给了不在职员工 %d", item.ID, emp.ID)
					}
					if emp.MaxConcurrent > 0 && emp.CurrentLoad >= emp.MaxConcurrent {
						t.Errorf("%s: 指派给了已达并发上限的员工 %d (%d/%d)",
							item.ID, emp.ID, emp.CurrentLoad, emp.MaxConcurrent)
					}
				}
			}
		})
	}
}

// TestEvalDiscriminatesSkillMatching 是本文件最重要的用例。
//
// 它证明评测具备真实区分能力：在对抗集上，把技能权重置零后
// 评测必须显著失败。若此用例失败（即置零后仍全绿），
// 说明「100% 准确率」并不能证明技能匹配在工作，评测框架就只是装饰品。
func TestEvalDiscriminatesSkillMatching(t *testing.T) {
	dataset := loadHardDataset(t)

	baseline := RunAssignmentEval(dataset, assign.New(assign.DefaultWeights()))
	if baseline.Passed != baseline.Total {
		t.Fatalf("基线在对抗集上应全绿，实际 %d/%d", baseline.Passed, baseline.Total)
	}

	broken := RunAssignmentEval(dataset, assign.NewWithoutSkillWeight())
	if broken.Passed == broken.Total {
		t.Fatal("把技能权重置零后对抗集仍全部通过 —— 评测无法证明技能匹配在工作")
	}
	// 要求下降幅度足够显著，避免只掉一两条就宣称有区分力。
	if ratio := float64(broken.Passed) / float64(broken.Total); ratio > 0.5 {
		t.Fatalf("区分力不足：置零技能权重后仍通过 %.0f%%，期望低于 50%%", ratio*100)
	}
	t.Logf("对抗集检出技能权重失效：通过率 %d/%d → %d/%d",
		baseline.Passed, baseline.Total, broken.Passed, broken.Total)
}

// TestEvalDetectsInvertedSelection 证明排序方向写反时可被检出。
func TestEvalDetectsInvertedSelection(t *testing.T) {
	dataset := loadHardDataset(t)
	broken := RunAssignmentEval(dataset, assign.NewWithInvertedSelection())

	if broken.Passed == broken.Total {
		t.Fatal("反转选择顺序后评测仍全部通过 —— 评测无法检测排序方向错误")
	}
	t.Logf("检出排序方向错误：通过率降至 %d/%d", broken.Passed, broken.Total)
}

func firstFailures(report AssignmentReport, limit int) []string {
	ret := make([]string, 0, limit)
	for i, failure := range report.Failures {
		if i >= limit {
			break
		}
		ret = append(ret, failure.CaseID+": "+failure.FailureMsg)
	}
	return ret
}
