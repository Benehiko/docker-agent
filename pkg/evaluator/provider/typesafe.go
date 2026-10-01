package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
)

const (
	maxResponseBytes = 1 << 20
	// The API may round each probability and the weighted score independently.
	roundingTolerance = 1e-3
)

type typesafe struct {
	client          *http.Client
	env             environment.Provider
	endpoint        string
	tokenKey        string
	model           string
	resultType      string
	questionType    string
	question        json.RawMessage
	probabilityKeys []string
	timeout         time.Duration
	cost            *latest.CostConfig
	officialPricing bool
}

type typesafeQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type typesafeRequest struct {
	Model     string                     `json:"model"`
	State     json.RawMessage            `json:"state"`
	Questions map[string]json.RawMessage `json:"questions"`
}

type typesafeResponse struct {
	Model   json.RawMessage `json:"model"`
	Answers json.RawMessage `json:"answers"`
	Usage   json.RawMessage `json:"usage"`
}

type typesafeAnswer struct {
	Type          string              `json:"type"`
	Noul          *float64            `json:"noul"`
	Choice        *string             `json:"choice"`
	Score         *float64            `json:"score"`
	Probabilities map[string]*float64 `json:"probabilities"`
	Confidence    *float64            `json:"confidence"`
}

func (p *typesafe) Evaluate(ctx context.Context, state any) (*evaluator.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rawState, err := json.Marshal(state)
	if err != nil {
		return nil, errors.New("evaluator state must be JSON-serializable")
	}
	if len(rawState) == 0 || (rawState[0] != '"' && rawState[0] != '{' && rawState[0] != '[') {
		return nil, errors.New("evaluator state must be a string, object, or array")
	}
	payload, err := json.Marshal(typesafeRequest{
		Model:     p.model,
		State:     rawState,
		Questions: map[string]json.RawMessage{"evaluation": p.question},
	})
	if err != nil {
		return nil, errors.New("failed to encode evaluator request")
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	token, ok := p.env.Get(ctx, p.tokenKey)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ok || strings.TrimSpace(token) == "" {
		return nil, errors.New("evaluator API key is missing")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("failed to construct evaluator request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	record := evaluator.UsageRecord{Model: p.model}
	defer func() { evaluator.ObserveUsage(ctx, record) }()
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, requestError(ctx, "evaluator request failed")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, requestError(ctx, "failed to read evaluator response")
	}
	if len(body) > maxResponseBytes {
		return nil, errors.New("evaluator response exceeds size limit")
	}
	var response typesafeResponse
	decodeErr := json.Unmarshal(body, &response)
	var model string
	modelErr := json.Unmarshal(response.Model, &model)
	if strings.TrimSpace(model) != "" {
		record.Model = model
	}
	var usageErr error
	if decodeErr == nil {
		record.Usage, usageErr = reportedUsage(response.Usage)
		record.Cost = p.estimateCost(model, record.Usage)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("evaluator returned HTTP status %d", resp.StatusCode)
	}
	if decodeErr != nil || (len(response.Model) != 0 && modelErr != nil) {
		return nil, errors.New("invalid evaluator response JSON")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("evaluator response is missing the model")
	}
	if usageErr != nil {
		// Accounting cannot be trusted, so this must end the run instead of falling back.
		return nil, &evaluator.TerminalError{Err: usageErr}
	}
	return p.result(response.Answers, record)
}

// Preserve cancellation identity without exposing transport errors or URLs.
func requestError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", message, err)
	}
	return errors.New(message)
}

func (p *typesafe) result(rawAnswers json.RawMessage, record evaluator.UsageRecord) (*evaluator.Result, error) {
	var answers map[string]*typesafeAnswer
	if len(rawAnswers) != 0 {
		if err := json.Unmarshal(rawAnswers, &answers); err != nil {
			return nil, errors.New("invalid evaluator response JSON")
		}
	}
	answer := answers["evaluation"]
	if answer == nil {
		return nil, errors.New("evaluator response is missing the evaluation answer")
	}
	if answer.Type != p.questionType {
		return nil, errors.New("evaluator answer type does not match the question")
	}
	if answer.Confidence != nil && !validProbability(answer.Confidence) {
		return nil, errors.New("evaluator answer has invalid confidence")
	}
	result := &evaluator.Result{
		Type:       p.resultType,
		Model:      record.Model,
		Confidence: answer.Confidence,
		Cost:       record.Cost,
	}
	if record.Usage != nil {
		result.Usage = *record.Usage
	}

	if p.resultType == "boolean" {
		if !validProbability(answer.Noul) {
			return nil, errors.New("evaluator answer has missing or invalid probability")
		}
		result.Probability = answer.Noul
		return result, nil
	}

	probabilities, err := p.probabilities(answer.Probabilities)
	if err != nil {
		return nil, err
	}
	result.Probabilities = probabilities
	switch p.resultType {
	case "choice":
		if answer.Choice == nil {
			return nil, errors.New("evaluator answer is missing the choice")
		}
		selected, ok := probabilities[*answer.Choice]
		if !ok {
			return nil, errors.New("evaluator answer contains an unknown choice")
		}
		for _, probability := range probabilities {
			if probability > selected {
				return nil, errors.New("evaluator choice is not a highest-probability option")
			}
		}
		result.Choice = *answer.Choice
	case "score":
		if answer.Score == nil || math.IsNaN(*answer.Score) || math.IsInf(*answer.Score, 0) || *answer.Score < -roundingTolerance || *answer.Score > float64(len(p.probabilityKeys)-1)+roundingTolerance {
			return nil, errors.New("evaluator answer has missing or invalid score")
		}
		result.Score = answer.Score
	}
	return result, nil
}

func (p *typesafe) probabilities(values map[string]*float64) (map[string]float64, error) {
	if len(values) != len(p.probabilityKeys) {
		return nil, errors.New("evaluator answer probability keys do not match the criteria")
	}
	probabilities := make(map[string]float64, len(values))
	var sum float64
	for _, key := range p.probabilityKeys {
		probability := values[key]
		if !validProbability(probability) {
			return nil, errors.New("evaluator answer has missing or invalid probabilities")
		}
		probabilities[key] = *probability
		sum += *probability
	}
	if math.Abs(sum-1) > roundingTolerance {
		return nil, errors.New("evaluator answer probabilities do not sum to one")
	}
	return probabilities, nil
}

func validProbability(value *float64) bool {
	return value != nil && !math.IsNaN(*value) && *value >= 0 && *value <= 1
}
