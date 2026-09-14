package domain

import (
	"sort"
	"strconv"
	"strings"
)

// SkillSet 技能集合。
//
// 用 map 而非切片：集合语义（去重、交并）是匹配的核心操作，
// 且技能数量级很小（十位以内），不需要为有序遍历做特殊优化。
type SkillSet map[int64]struct{}

// NewSkillSet 从技能 ID 列表构造集合，自动去重并丢弃非正数。
func NewSkillSet(ids ...int64) SkillSet {
	ret := make(SkillSet, len(ids))
	for _, id := range ids {
		if id > 0 {
			ret[id] = struct{}{}
		}
	}
	return ret
}

// IDs 返回升序排列的技能 ID，用于稳定的日志与测试断言。
func (s SkillSet) IDs() []int64 {
	if len(s) == 0 {
		return nil
	}
	ret := make([]int64, 0, len(s))
	for id := range s {
		ret = append(ret, id)
	}
	sort.Slice(ret, func(i, j int) bool { return ret[i] < ret[j] })
	return ret
}

// IsEmpty 判断集合是否为空。
func (s SkillSet) IsEmpty() bool { return len(s) == 0 }

// Has 判断集合是否包含指定技能。
func (s SkillSet) Has(id int64) bool {
	_, ok := s[id]
	return ok
}

// Intersection 返回与 other 的交集。
func (s SkillSet) Intersection(other SkillSet) SkillSet {
	if len(s) == 0 || len(other) == 0 {
		return SkillSet{}
	}
	// 遍历较小的一侧以减少比较次数。
	small, large := s, other
	if len(large) < len(small) {
		small, large = large, small
	}
	ret := make(SkillSet)
	for id := range small {
		if _, ok := large[id]; ok {
			ret[id] = struct{}{}
		}
	}
	return ret
}

// Union 返回与 other 的并集。
func (s SkillSet) Union(other SkillSet) SkillSet {
	ret := make(SkillSet, len(s)+len(other))
	for id := range s {
		ret[id] = struct{}{}
	}
	for id := range other {
		ret[id] = struct{}{}
	}
	return ret
}

// Jaccard 计算与 other 的 Jaccard 相似度 |A∩B| / |A∪B|，取值 0..1。
//
// 两侧皆空时返回 0 而非 1：空技能需求不应被视为「完美匹配任何员工」，
// 否则无技能的工单会随机命中，掩盖匹配失效。
func (s SkillSet) Jaccard(other SkillSet) float64 {
	if len(s) == 0 || len(other) == 0 {
		return 0
	}
	inter := len(s.Intersection(other))
	if inter == 0 {
		return 0
	}
	union := len(s.Union(other))
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// CoveredBy 返回 s 中被 other 覆盖的比例，取值 0..1。
//
// 与 Jaccard 的区别：只以 s 自身为分母，衡量「需求被满足了多少」，
// 不惩罚员工技能过多。用于评测中区分「专才」与「通才」。
func (s SkillSet) CoveredBy(other SkillSet) float64 {
	if len(s) == 0 {
		return 0
	}
	return float64(len(s.Intersection(other))) / float64(len(s))
}

// ParseSkillIDs 解析逗号分隔的技能 ID 列表，忽略空白与非法项。
func ParseSkillIDs(raw string) []int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	ret := make([]int64, 0, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		ret = append(ret, id)
	}
	return ret
}

// ContainsAll 判断 s 是否包含 required 中的全部技能。
func (s SkillSet) ContainsAll(required SkillSet) bool {
	if required.IsEmpty() {
		return true
	}
	for id := range required {
		if !s.Has(id) {
			return false
		}
	}
	return true
}
