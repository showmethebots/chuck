package triage

import "fmt"

// Usage is what one classifier call consumed.
type Usage struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
}

// pricesPerMTok are USD per million input and output tokens.
var pricesPerMTok = map[string][2]float64{
	HaikuModel: {1, 5},
}

// CostUSD prices a call. An unknown model is an error rather than free, so a
// model change cannot silently bypass the daily budget.
func CostUSD(u Usage) (float64, error) {
	p, ok := pricesPerMTok[u.Model]
	if !ok {
		return 0, fmt.Errorf("no price for model %q", u.Model)
	}
	return (float64(u.InputTokens)*p[0] + float64(u.OutputTokens)*p[1]) / 1e6, nil
}
