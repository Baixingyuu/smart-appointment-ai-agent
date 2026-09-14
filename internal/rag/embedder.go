package rag

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"strings"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// HashEmbedder 是确定性的离线向量化实现。
//
// 用途：让检索、证据判定、工具调用等全部逻辑可在无网络、无 API Key、
// 零成本的情况下被确定性地测试与演示。它不是语义向量——
// 只依据字符 bigram 做哈希投影，同义不同词无法匹配。
//
// 因此它只适用于「验证流程正确」与「离线演示」，生产必须换成模型向量化。
// 这一点在 README 与启动日志中都会明确标注，避免被误当作真实语义检索。
type HashEmbedder struct {
	dim int
}

// NewHashEmbedder 构造离线向量化器。dim <= 0 时使用默认维度。
func NewHashEmbedder(dim int) *HashEmbedder {
	if dim <= 0 {
		dim = 256
	}
	return &HashEmbedder{dim: dim}
}

// Dimension 返回向量维度。
func (h *HashEmbedder) Dimension() int { return h.dim }

// Embed 把文本投影为定长向量。
//
// 用 bigram 哈希到固定桶并累加计数，再做 L2 归一化。
// 相同输入必然得到相同输出，因此检索结果可复现。
func (h *HashEmbedder) Embed(text string) []float64 {
	vector := make([]float64, h.dim)
	terms := bigrams(text)
	if len(terms) == 0 {
		return vector
	}
	for term := range terms {
		hash := fnv.New32a()
		_, _ = hash.Write([]byte(term))
		index := int(hash.Sum32() % uint32(h.dim))
		vector[index]++
	}
	// L2 归一化，使余弦相似度不受文本长度影响。
	var norm float64
	for _, value := range vector {
		norm += value * value
	}
	if norm == 0 {
		return vector
	}
	norm = math.Sqrt(norm)
	for i := range vector {
		vector[i] /= norm
	}
	return vector
}

// OpenAIEmbedder 基于官方 SDK 的向量化实现。
type OpenAIEmbedder struct {
	client openai.Client
	model  string
	dim    int
}

// EmbedderConfig 向量化配置。
type EmbedderConfig struct {
	BaseURL string
	APIKey  string
	Model   string
	// Dimension 为预期向量维度，仅用于文档与校验；0 表示未知。
	Dimension int
}

// NewOpenAIEmbedder 构造模型向量化器。
func NewOpenAIEmbedder(config EmbedderConfig) (*OpenAIEmbedder, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, fmt.Errorf("缺少 API Key")
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, fmt.Errorf("缺少 embedding 模型名称")
	}
	opts := []option.RequestOption{option.WithAPIKey(config.APIKey)}
	if strings.TrimSpace(config.BaseURL) != "" {
		opts = append(opts, option.WithBaseURL(strings.TrimSpace(config.BaseURL)))
	}
	return &OpenAIEmbedder{
		client: openai.NewClient(opts...),
		model:  strings.TrimSpace(config.Model),
		dim:    config.Dimension,
	}, nil
}

// Dimension 返回已知的向量维度；未知时为 0。
func (o *OpenAIEmbedder) Dimension() int { return o.dim }

// Embed 调用模型接口完成向量化。
//
// 该接口是同步的且无 ctx 参数（Embedder 接口不含 context），
// 因此用 Background 并在调用方通过超时配置控制。
// 若将来需要严格的取消语义，应把 context 提升到接口签名上。
func (o *OpenAIEmbedder) Embed(text string) []float64 {
	response, err := o.client.Embeddings.New(context.Background(), openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel(o.model),
		Input: openai.EmbeddingNewParamsInputUnion{OfString: openai.String(text)},
	})
	if err != nil || len(response.Data) == 0 {
		// 返回空向量而非 panic：调用方会因向量为空而命中「未配置向量化」
		// 分支并给出可读提示，比让进程崩溃更可控。
		return nil
	}
	embedding := response.Data[0].Embedding
	if o.dim == 0 {
		o.dim = len(embedding)
	}
	return embedding
}

// 确保两个实现都满足接口。
var (
	_ Embedder = (*HashEmbedder)(nil)
	_ Embedder = (*OpenAIEmbedder)(nil)
)
