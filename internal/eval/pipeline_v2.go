// 三段流水线的评测 runner。
//
// 三段拆成三个独立轴各打一次分，而不是压成一个通过率：混在一起时
// 任何一段回归都会污染其他段的读数，失败也无法归因。
//
// 数据集 schema 见 eval/datasets/gen_v2.py。三段金标全部规则派生，本 runner 证明的是
// "pipeline 实现 == 我们宣称的规则"，不是外部准确率；对外准确率声明需要挂人工标注卡。
//
// 设计文档：docs/DISPATCH_PIPELINE.md §5.2。
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/domain"
)

// PipelineV2Source 记录一条 v2 样本的来源；开源蓝本要可追溯。
type PipelineV2Source struct {
	Kind           string `json:"kind"` // console-ai | handcrafted
	OriginID       string `json:"originId,omitempty"`
	OriginCategory string `json:"originCategory,omitempty"`
	OriginPriority string `json:"originPriority,omitempty"`
	OriginSubject  string `json:"originSubject,omitempty"`
}

// PipelineV2Ticket 待派单工单的原文；只给文本，服务解析由 pipeline 自己完成。
type PipelineV2Ticket struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Priority    string `json:"priority"`
}

// PipelineV2Expect 三段金标。
//
// Outcome=matched 时 AssigneeID 必须 >0；其他 outcome 下固定 0，runner 不比对排序段。
// ServiceTop1=0 表示"接受任何低于 ServiceMatchFloor 的 top-1"，用于 no_service_match 类样本。
type PipelineV2Expect struct {
	ServiceTop1 int64  `json:"serviceTop1"`
	Weakness    string `json:"weakness"`
	Outcome     string `json:"outcome"`
	AssigneeID  int64  `json:"assigneeId"`
}

// PipelineV2Case 单条 v2 样本。
type PipelineV2Case struct {
	ID       string           `json:"id"`
	Source   PipelineV2Source `json:"source"`
	Ticket   PipelineV2Ticket `json:"ticket"`
	Expected PipelineV2Expect `json:"expected"`
	Notes    string           `json:"notes"`
}

// PipelineV2Provenance 记录数据集蓝本出处，让"改造自 XX 数据集"可核验。
type PipelineV2Provenance struct {
	Primary     PipelineV2ProvenanceEntry `json:"primary"`
	SanityCheck PipelineV2ProvenanceEntry `json:"sanity_check"`
}

type PipelineV2ProvenanceEntry struct {
	Name    string `json:"name"`
	License string `json:"license"`
	URL     string `json:"url"`
	Use     string `json:"use"`
}

// PipelineV2Dataset 完整数据集。
type PipelineV2Dataset struct {
	Version      int                  `json:"version"`
	PipelineAxes bool                 `json:"pipelineAxes"`
	Description  string               `json:"description"`
	Provenance   PipelineV2Provenance `json:"provenance"`
	GoldSource   string               `json:"goldSource"`
	GoldBoundary string               `json:"goldBoundary"`
	Cases        []PipelineV2Case     `json:"cases"`
}

// LoadPipelineV2Dataset 从 JSON 文件加载 v2 数据集。
func LoadPipelineV2Dataset(path string) (*PipelineV2Dataset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read dataset %s: %w", path, err)
	}
	var ds PipelineV2Dataset
	if err := json.Unmarshal(data, &ds); err != nil {
		return nil, fmt.Errorf("parse dataset %s: %w", path, err)
	}
	if len(ds.Cases) == 0 {
		return nil, fmt.Errorf("dataset %s has no cases", path)
	}
	return &ds, nil
}

// PipelineV2CaseResult 单条样本的对比明细。
//
// 三段各自 pass 与 total 一起记：这样能算出"抽取对了但排序错了"这种部分失败的样本占比，
// 而不是把三段压成一个布尔值。
type PipelineV2CaseResult struct {
	CaseID         string `json:"caseId"`
	ExpectedTop1   int64  `json:"expectedTop1"`
	ActualTop1     int64  `json:"actualTop1"`
	ExpectedWeak   string `json:"expectedWeakness"`
	ActualWeak     string `json:"actualWeakness"`
	ExpectedOut    string `json:"expectedOutcome"`
	ActualOut      string `json:"actualOutcome"`
	ExpectedAssign int64  `json:"expectedAssignee"`
	ActualAssign   int64  `json:"actualAssignee"`
	ActualPath     string `json:"actualPath"`
	ExtractPass    bool   `json:"extractPass"`
	WeakPass       bool   `json:"weakPass"`
	SortPass       bool   `json:"sortPass"`
	SortApplicable bool   `json:"sortApplicable"` // 只在 gold outcome=matched 时评
	Reason         string `json:"reason,omitempty"`
}

// PipelineV2Axis 一个轴的汇总。
type PipelineV2Axis struct {
	Applicable int     `json:"applicable"`
	Pass       int     `json:"pass"`
	Accuracy   float64 `json:"accuracy"`
}

// PipelineV2Funnel Stage 分布 —— 三段流水线设计的核心可观测目标 (80/15/5)。
type PipelineV2Funnel struct {
	Stage1Direct int `json:"stage1Direct"` // Path = stage1_*
	Stage2       int `json:"stage2"`       // Path = stage2_*
	Stage3       int `json:"stage3"`       // Path = stage3_*
	Total        int `json:"total"`
}

// PipelineV2Report 完整评测报告。
type PipelineV2Report struct {
	Dataset    string                 `json:"dataset"`
	Total      int                    `json:"total"`
	Extraction PipelineV2Axis         `json:"extraction"`
	Weakness   PipelineV2Axis         `json:"weakness"`
	Sort       PipelineV2Axis         `json:"sort"`
	Funnel     PipelineV2Funnel       `json:"funnel"`
	Results    []PipelineV2CaseResult `json:"results"`
	Failures   []PipelineV2CaseResult `json:"failures,omitempty"`
}

// RunPipelineV2Eval 逐条跑 pipeline 并按三段各自打分。
//
// dir 提供 employees / extensions / services；调用方负责保证负载起点一致
// （否则同一 case 两次跑会因负载漂移落到不同员工）。传 seed 快照即可。
//
// pipeline chooser 为 nil 时判弱样本直接落 Stage 3 —— 这条路径的期望是
// Weakness 与 Outcome 命中；Stage 2 单独评测请注入 ScriptedChooser。
func RunPipelineV2Eval(ds *PipelineV2Dataset, p *assign.Pipeline, dir assign.Directory) PipelineV2Report {
	rep := PipelineV2Report{Total: len(ds.Cases)}
	for _, c := range ds.Cases {
		res := runOneV2(c, p, dir)
		rep.Results = append(rep.Results, res)
		rep.Extraction.Applicable++
		if res.ExtractPass {
			rep.Extraction.Pass++
		}
		rep.Weakness.Applicable++
		if res.WeakPass {
			rep.Weakness.Pass++
		}
		if res.SortApplicable {
			rep.Sort.Applicable++
			if res.SortPass {
				rep.Sort.Pass++
			}
		}
		switch {
		case strings.HasPrefix(res.ActualPath, "stage1_"):
			rep.Funnel.Stage1Direct++
		case strings.HasPrefix(res.ActualPath, "stage2_"):
			rep.Funnel.Stage2++
		case strings.HasPrefix(res.ActualPath, "stage3_"):
			rep.Funnel.Stage3++
		}
		if !res.ExtractPass || !res.WeakPass || (res.SortApplicable && !res.SortPass) {
			rep.Failures = append(rep.Failures, res)
		}
	}
	rep.Funnel.Total = len(ds.Cases)
	finishAxis(&rep.Extraction)
	finishAxis(&rep.Weakness)
	finishAxis(&rep.Sort)
	return rep
}

func finishAxis(a *PipelineV2Axis) {
	if a.Applicable == 0 {
		a.Accuracy = 0
		return
	}
	a.Accuracy = float64(a.Pass) / float64(a.Applicable)
}

func runOneV2(c PipelineV2Case, p *assign.Pipeline, dir assign.Directory) PipelineV2CaseResult {
	tk := domain.Ticket{
		ID:          int64(hashCaseID(c.ID)),
		Title:       c.Ticket.Title,
		Description: c.Ticket.Description,
		Category:    domain.Category(c.Ticket.Category),
		Priority:    domain.Priority(c.Ticket.Priority),
		Status:      domain.TicketStatusPending,
	}
	dec, err := p.Run(context.Background(), tk, dir)
	res := PipelineV2CaseResult{
		CaseID:         c.ID,
		ExpectedTop1:   c.Expected.ServiceTop1,
		ExpectedWeak:   c.Expected.Weakness,
		ExpectedOut:    c.Expected.Outcome,
		ExpectedAssign: c.Expected.AssigneeID,
		ActualWeak:     dec.Weakness.String(),
		ActualOut:      outcomeToString(dec.Outcome),
		ActualPath:     string(dec.Path),
		ActualAssign:   dec.AssigneeID,
	}
	if len(dec.Services) > 0 {
		res.ActualTop1 = dec.Services[0].ServiceID
	}
	// ServiceTop1 = 0 表示"接受任何 < floor 的 top-1"。
	// 判据用 weakness 而不是分数绝对值：weakness=no_service_match 已经隐含"分不够"。
	if c.Expected.ServiceTop1 == 0 {
		res.ExtractPass = dec.Weakness == assign.WeaknessNoServiceMatch ||
			len(dec.Services) == 0
	} else {
		res.ExtractPass = res.ActualTop1 == c.Expected.ServiceTop1
	}
	res.WeakPass = dec.Weakness.String() == c.Expected.Weakness
	res.SortApplicable = c.Expected.Outcome == "matched"
	if res.SortApplicable {
		res.SortPass = dec.AssigneeID == c.Expected.AssigneeID
	}
	if err != nil {
		res.Reason = fmt.Sprintf("pipeline error: %v", err)
	}
	return res
}

// outcomeToString 与 domain.AssignmentOutcome 对齐；小写串与数据集 expect 字段一致。
func outcomeToString(o domain.AssignmentOutcome) string {
	return string(o)
}

// hashCaseID 给 case 一个稳定的 int64 ID，供 Decision 内部使用；
// pipeline 只把它当工单主键用，不参与打分。
func hashCaseID(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// Text 输出人类可读报告。
//
// 失败列表按 case id 排序，方便与 gen_v2.py 里的 case 顺序对齐复核。
func (r PipelineV2Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "pipeline v2 评测：%s（%d 条）\n", r.Dataset, r.Total)
	fmt.Fprintf(&b, "  抽取轴 top-1: %d/%d = %.2f%%\n", r.Extraction.Pass, r.Extraction.Applicable, r.Extraction.Accuracy*100)
	fmt.Fprintf(&b, "  判弱轴 类别一致: %d/%d = %.2f%%\n", r.Weakness.Pass, r.Weakness.Applicable, r.Weakness.Accuracy*100)
	fmt.Fprintf(&b, "  排序轴 winner: %d/%d = %.2f%%\n", r.Sort.Pass, r.Sort.Applicable, r.Sort.Accuracy*100)
	fmt.Fprintf(&b, "  漏斗: stage1=%d stage2=%d stage3=%d 总=%d\n",
		r.Funnel.Stage1Direct, r.Funnel.Stage2, r.Funnel.Stage3, r.Funnel.Total)
	if len(r.Failures) == 0 {
		b.WriteString("  ✓ 三段全部命中金标\n")
		return b.String()
	}
	sort.SliceStable(r.Failures, func(i, j int) bool { return r.Failures[i].CaseID < r.Failures[j].CaseID })
	fmt.Fprintf(&b, "  ✗ %d 条与金标不一致（供逐条复核）:\n", len(r.Failures))
	for _, f := range r.Failures {
		flags := []string{}
		if !f.ExtractPass {
			flags = append(flags, fmt.Sprintf("extract(exp=%d got=%d)", f.ExpectedTop1, f.ActualTop1))
		}
		if !f.WeakPass {
			flags = append(flags, fmt.Sprintf("weak(exp=%s got=%s)", f.ExpectedWeak, f.ActualWeak))
		}
		if f.SortApplicable && !f.SortPass {
			flags = append(flags, fmt.Sprintf("sort(exp=%d got=%d)", f.ExpectedAssign, f.ActualAssign))
		}
		fmt.Fprintf(&b, "    %s path=%s %s\n", f.CaseID, f.ActualPath, strings.Join(flags, " "))
	}
	return b.String()
}
