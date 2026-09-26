// Package agent 里的可追溯性判定组件。
//
// 它回答一个贯穿建单交互三处需求的问题：某段文本能不能追溯到用户说过的话。
// 三处需求共用这一个组件，口径天然一致：
//  1. extracted_slots 的 quote 是否真出自用户原话；
//  2. description 里每条事实声明是否有用户依据；
//  3. quote 的部分匹配容错（小模型给不出逐字 quote）。
//
// 两级漏斗与派单三段同一思想——不为先进，为省：便宜且确定性的规则先筛，
// 只有规则没命中的小集合才走语义模型。默认（scripted / 离线评测）只跑第一级，
// 第二级恒判 unverified，退化不报错——与既有 scripted/live 双模约定一致。
package agent

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/mac/helpdesk-agent/internal/llm"
)

// Verdict 一条声明的可追溯结论。
type Verdict string

const (
	// VerdictTraced 字面可追溯：归一化后子串命中或词元重叠率达标。
	VerdictTraced Verdict = "traced"
	// VerdictEntailed 语义可推出：规则没命中，但模型判定用户原话能蕴含该声明。
	VerdictEntailed Verdict = "entailed"
	// VerdictUnverified 拿不准：保守标注，不据此认定"属实"。
	VerdictUnverified Verdict = "unverified"
)

// Claim 一条待判定的声明。ID 用于把批量结果对齐回输入顺序。
type Claim struct {
	ID   string
	Text string
}

// ClaimVerdict 一条声明的判定结果。
type ClaimVerdict struct {
	ID      string
	Verdict Verdict
	Reason  string
}

// TraceabilityChecker 判断一批 claim 能否追溯到 userCorpus（本会话用户原话集合）。
//
// 刻意批量而非逐条：description 拆出的多条声明与多个槽位 quote 往往在同一次
// 建单里一起判定，批量给实现留出"合并成一次模型调用"的优化空间。
// userCorpus 由调用方传入，组件自身不读存储——保持纯、可测、无隐藏 I/O。
type TraceabilityChecker interface {
	CheckAll(ctx context.Context, claims []Claim, userCorpus []string) ([]ClaimVerdict, error)
}

// TraceabilityConfig 调整判定阈值。
type TraceabilityConfig struct {
	// TokenOverlapThreshold 是词元重叠率判 traced 的门限，取值 (0,1]。
	// 0.70 是拍的：真值要靠人肉抽检标定，见 TICKET_INTAKE §10 的触发条件。
	TokenOverlapThreshold float64
}

// DefaultTraceabilityConfig 返回默认阈值。
func DefaultTraceabilityConfig() TraceabilityConfig {
	return TraceabilityConfig{TokenOverlapThreshold: 0.70}
}

// scriptableTraceability 是第一级规则判定的免费实现，也是离线评测用的 scripted 版：
// 只做字面/词元匹配，规则没命中一律 unverified（无第二级语义模型）。
type scriptableTraceability struct {
	cfg TraceabilityConfig
}

// NewScriptedTraceabilityChecker 构造 scripted（规则级）判定器。
// 无模型时用它：退化行为明确（规则没过即 unverified），绝不静默判"属实"。
func NewScriptedTraceabilityChecker() TraceabilityChecker {
	return scriptableTraceability{cfg: DefaultTraceabilityConfig()}
}

// NewRuleTraceabilityChecker 用给定阈值构造 scripted（规则级）判定器，供测试调阈值。
func NewRuleTraceabilityChecker(cfg TraceabilityConfig) TraceabilityChecker {
	return scriptableTraceability{cfg: cfg}
}

// twoLevelTraceability 在规则级之上，把规则没命中的 claim 交给语义模型再判一次。
type twoLevelTraceability struct {
	rule  scriptableTraceability
	model llm.ChatModel
}

// NewTwoLevelTraceabilityChecker 构造两级判定器。
//
// model 为 nil 时等价于 scripted（规则级）——不 panic，明确退化。
// 注意：一期第二级对每条未命中 claim 各调一次模型。批量合并成一次调用是
// 已识别的成本优化（见 TICKET_INTAKE §3.2），一期用本地模型验正确性，不承诺成本数字。
func NewTwoLevelTraceabilityChecker(model llm.ChatModel, cfg TraceabilityConfig) TraceabilityChecker {
	rule := scriptableTraceability{cfg: cfg}
	if model == nil {
		return rule
	}
	return twoLevelTraceability{rule: rule, model: model}
}

// CheckAll 实现 TraceabilityChecker：纯规则级。
func (r scriptableTraceability) CheckAll(_ context.Context, claims []Claim, userCorpus []string) ([]ClaimVerdict, error) {
	corpus := normalizeForMatch(strings.Join(userCorpus, "\n"))
	corpusTokens := tokenSet(corpus)
	out := make([]ClaimVerdict, 0, len(claims))
	for _, claim := range claims {
		out = append(out, r.judge(claim.ID, claim.Text, corpus, corpusTokens))
	}
	return out, nil
}

// judge 跑第一级规则，返回 traced 或 unverified。
func (r scriptableTraceability) judge(id, text, corpus string, corpusTokens map[string]struct{}) ClaimVerdict {
	normalized := normalizeForMatch(text)
	if normalized == "" {
		return ClaimVerdict{ID: id, Verdict: VerdictUnverified, Reason: "claim 归一化后为空"}
	}
	// 子串命中：整段声明原样出现在用户语料里。
	if strings.Contains(corpus, normalized) {
		return ClaimVerdict{ID: id, Verdict: VerdictTraced, Reason: "归一化后子串命中用户原话"}
	}
	rate := coverageRate(normalized, corpusTokens)
	if rate >= r.cfg.TokenOverlapThreshold {
		return ClaimVerdict{ID: id, Verdict: VerdictTraced, Reason: fmt.Sprintf("词元重叠率 %.2f ≥ 阈值", rate)}
	}
	return ClaimVerdict{ID: id, Verdict: VerdictUnverified, Reason: fmt.Sprintf("词元重叠率 %.2f 未达阈值", rate)}
}

// CheckAll 实现 TraceabilityChecker：规则先筛，规则没过的逐条走语义模型。
func (t twoLevelTraceability) CheckAll(ctx context.Context, claims []Claim, userCorpus []string) ([]ClaimVerdict, error) {
	ruleVerdicts, err := t.rule.CheckAll(ctx, claims, userCorpus)
	if err != nil {
		return nil, err
	}
	premise := strings.TrimSpace(strings.Join(userCorpus, "\n"))
	if premise == "" {
		// 没有用户原话可依据：规则级已给出结论（多半全 unverified），不再问模型。
		return ruleVerdicts, nil
	}
	for i, verdict := range ruleVerdicts {
		if verdict.Verdict != VerdictUnverified {
			continue
		}
		claim := claims[i]
		if entailed := t.entailedByModel(ctx, premise, claim.Text); entailed {
			ruleVerdicts[i] = ClaimVerdict{ID: verdict.ID, Verdict: VerdictEntailed, Reason: "语义模型判定可由用户原话推出"}
		}
	}
	return ruleVerdicts, nil
}

const entailmentSystemPrompt = `你是文本蕴含判定器。给你"用户原话"（前提）与一条"待核声明"（假设）。
只输出一个词：
ENTAIL —— 假设能由前提直接推出或等价复述；
NO —— 前提推不出假设、与前提矛盾、或前提没有相关信息。
不要解释，不要输出除 ENTAIL / NO 以外的内容。`

// entailedByModel 问一次模型：给定前提，这条声明能否推出。
//
// 任何失败（调用出错、响应不可解析）都按"推不出"处理，落 unverified。
// 方向刻意保守：判定用于"标注可能编造"，宁可多标注也不放过编造。
func (t twoLevelTraceability) entailedByModel(ctx context.Context, premise, hypothesis string) bool {
	user := fmt.Sprintf("【用户原话】\n%s\n\n【待核声明】\n%s", capRunes(premise, 1200), capRunes(hypothesis, 400))
	resp, err := t.model.Chat(ctx, entailmentSystemPrompt, user)
	if err != nil || resp == nil {
		return false
	}
	normalized := strings.ToUpper(strings.TrimSpace(resp.Content))
	return strings.Contains(normalized, "ENTAIL")
}

// normalizeForMatch 归一化：去掉空白与标点符号，只保留字母数字与文字，ASCII 转小写。
//
// 中文没有词边界，去掉标点/空格后按字符级比较即可覆盖"全半角、多空格、
// 句号有无"这类噪声；英文统一小写覆盖大小写差异。
func normalizeForMatch(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if r < unicode.MaxASCII {
				b.WriteRune(unicode.ToLower(r))
			} else {
				b.WriteRune(r)
			}
		default:
			// 标点、空格、符号一律丢弃。
		}
	}
	return b.String()
}

// tokenize 把归一化后的文本切成词元：ASCII 连续字母数字为一个词元，
// 其余（中日韩等）按单字成元。中文单字近似词，够做重叠率。
func tokenize(normalized string) []string {
	var tokens []string
	var ascii []rune
	flush := func() {
		if len(ascii) > 0 {
			tokens = append(tokens, string(ascii))
			ascii = ascii[:0]
		}
	}
	for _, r := range normalized {
		if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			ascii = append(ascii, r)
			continue
		}
		flush()
		tokens = append(tokens, string(r))
	}
	flush()
	return tokens
}

func tokenSet(normalized string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, tok := range tokenize(normalized) {
		set[tok] = struct{}{}
	}
	return set
}

// coverageRate 返回 claim 的词元有多大比例出现在语料词元集里（0..1）。
// 分母是 claim 自身词元数——衡量"这条声明有多少被用户原话撑着"。
func coverageRate(claimNormalized string, corpusTokens map[string]struct{}) float64 {
	toks := tokenize(claimNormalized)
	if len(toks) == 0 {
		return 0
	}
	hit := 0
	for _, tok := range toks {
		if _, ok := corpusTokens[tok]; ok {
			hit++
		}
	}
	return float64(hit) / float64(len(toks))
}
