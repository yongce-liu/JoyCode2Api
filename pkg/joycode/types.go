package joycode

// ModelInfo describes a JoyCode AI model.
// ModelExt 对应 modelList 中 extJson 字段（GPT 系模型的 adapter 元数据）。
type ModelExt struct {
	AdapterType string `json:"adapterType"`
	Reasoning   struct {
		DefaultEffort string `json:"defaultEffort"`
	} `json:"reasoning"`
}

type ModelInfo struct {
	Label              string    `json:"label"`
	ChatAPIModel       string    `json:"chatApiModel"`
	MaxTotalTokens     int       `json:"maxTotalTokens"`
	RespMaxTokens      int       `json:"respMaxTokens"`
	Temperature        float64   `json:"temperature"`
	Features           []string  `json:"features"`
	SupportStream      bool      `json:"supportStream"`
	VerificationStatus string    `json:"verificationStatus"`
	ModelID            string    `json:"modelId"`
	CreateTime         int64     `json:"createTime"`
	ExtJson            *ModelExt `json:"extJson"`
}

// SupportsResponses 判断模型是否走 openai-responses adapter（如 GPT-6 系）。
func (m ModelInfo) SupportsResponses() bool {
	return m.ExtJson != nil && m.ExtJson.AdapterType == "openai-response"
}

// ID is the callable model identifier; labels are only a last-resort fallback.
func (m ModelInfo) ID() string {
	if m.ModelID != "" {
		return m.ModelID
	}
	if m.ChatAPIModel != "" {
		return m.ChatAPIModel
	}
	return m.Label
}
