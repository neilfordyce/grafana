package state

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/grafana/grafana/pkg/expr"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/ngalert/eval"
	"github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/screenshot"
)

type State struct {
	OrgID        int64
	AlertRuleUID string

	// CacheID is a unique, opaque identifier for the state, and is used to find the state
	// in the state cache. It tends to be derived from the state's labels.
	CacheID string

	// State represents the current state.
	State eval.State

	// StateReason is a textual description to explain why the state has its current state.
	StateReason string

	// Results contains the result of the current and previous evaluations.
	Results []Evaluation

	// Error is set if the current evaluation returned an error. If error is non-nil results
	// can still contain the results of previous evaluations.
	Error error

	// Resolved is set to true if this state is the transitional state between Firing and Normal.
	// All subsequent states will be false until the next transition from Firing to Normal.
	Resolved bool

	// Image contains an optional image for the state. It tends to be included in notifications
	// as a visualization to show why the alert fired.
	Image *models.Image

	// Annotations contains the annotations from the alert rule. If an annotation is templated
	// then the template is first evaluated to derive the final annotation.
	Annotations map[string]string

	// Labels contain the labels from the query and any custom labels from the alert rule.
	// If a label is templated then the template is first evaluated to derive the final label.
	Labels data.Labels

	// Values contains the values of any instant vectors, reduce and math expressions, or classic
	// conditions.
	Values map[string]float64

	StartsAt             time.Time
	EndsAt               time.Time
	LastSentAt           time.Time
	LastEvaluationString string
	LastEvaluationTime   time.Time
	EvaluationDuration   time.Duration
}

func (a *State) GetRuleKey() models.AlertRuleKey {
	return models.AlertRuleKey{
		OrgID: a.OrgID,
		UID:   a.AlertRuleUID,
	}
}

func (a *State) GetAlertInstanceKey() (models.AlertInstanceKey, error) {
	instanceLabels := models.InstanceLabels(a.Labels)
	_, labelsHash, err := instanceLabels.StringAndHash()
	if err != nil {
		return models.AlertInstanceKey{}, err
	}
	return models.AlertInstanceKey{RuleOrgID: a.OrgID, RuleUID: a.AlertRuleUID, LabelsHash: labelsHash}, nil
}

func (a *State) Resolve(reason string, endsAt time.Time) {
	a.State = eval.Normal
	a.StateReason = reason
	a.EndsAt = endsAt
	a.Resolved = true
}

// StateTransition describes the transition from one state to another.
type StateTransition struct {
	*State
	PreviousState       eval.State
	PreviousStateReason string
}

func (c StateTransition) Formatted() string {
	return FormatStateAndReason(c.State.State, c.State.StateReason)
}

func (c StateTransition) PreviousFormatted() string {
	return FormatStateAndReason(c.PreviousState, c.PreviousStateReason)
}

func (c StateTransition) Changed() bool {
	return c.PreviousState != c.State.State || c.PreviousStateReason != c.State.StateReason
}

type Evaluation struct {
	EvaluationTime  time.Time
	EvaluationState eval.State
	// Values contains the RefID and value of reduce and math expressions.
	// Classic conditions can have different values for the same RefID as they can include multiple conditions.
	// For these, we use the index of the condition in addition RefID as the key e.g. "A0, A1, A2, etc.".
	Values map[string]*float64
	// Condition is the refID specified as the condition in the alerting rule at the time of the evaluation.
	Condition string
}

// NewEvaluationValues returns the labels and values for each RefID in the capture.
func NewEvaluationValues(m map[string]eval.NumberValueCapture) map[string]*float64 {
	result := make(map[string]*float64, len(m))
	for k, v := range m {
		result[k] = v.Value
	}
	return result
}

func resultNormal(state *State, _ *models.AlertRule, result eval.Result, logger log.Logger) {
	state.Error = nil // should be nil since state is not error

	if state.State != eval.Normal {
		logger.Debug("Changing state", "previous_state", state.State, "next_state", eval.Normal)
		state.State = eval.Normal
		state.StartsAt = result.EvaluatedAt
		state.EndsAt = result.EvaluatedAt
	}
}

func resultAlerting(state *State, rule *models.AlertRule, result eval.Result, logger log.Logger) {
	state.Error = result.Error

	switch state.State {
	case eval.Alerting:
		// If the previous state is Alerting then update the expiration time
		state.setEndsAt(rule, result)
	case eval.Pending:
		// If the previous state is Pending then check if the For duration has been observed
		if result.EvaluatedAt.Sub(state.StartsAt) >= rule.For {
			logger.Debug("Changing state", "previous_state", state.State, "next_state", eval.Alerting)
			state.State = eval.Alerting
			state.StartsAt = result.EvaluatedAt
			state.setEndsAt(rule, result)
		}
	default:
		if rule.For > 0 {
			// If the alert rule has a For duration that should be observed then the state should be set to Pending
			logger.Debug("Changing state", "previous_state", state.State, "next_state", eval.Pending)
			state.State = eval.Pending
		} else {
			logger.Debug("Changing state", "previous_state", state.State, "next_state", eval.Alerting)
			state.State = eval.Alerting
		}
		state.StartsAt = result.EvaluatedAt
		state.setEndsAt(rule, result)
	}
}

func resultError(state *State, rule *models.AlertRule, result eval.Result, logger log.Logger) {
	switch rule.ExecErrState {
	case models.AlertingErrState:
		resultAlerting(state, rule, result, logger)
	case models.ErrorErrState:
		state.Error = result.Error
		if result.Error != nil {
			// If the evaluation failed because a query returned an error then add the Ref ID and
			// Datasource UID as labels
			var queryError expr.QueryError
			if errors.As(state.Error, &queryError) {
				for _, next := range rule.Data {
					if next.RefID == queryError.RefID {
						state.Labels["ref_id"] = next.RefID
						state.Labels["datasource_uid"] = next.DatasourceUID
						break
					}
				}
				state.Annotations["Error"] = queryError.Error()
			}
		}

		if state.State == eval.Error {
			// If the previous state is Error then update the expiration time
			state.setEndsAt(rule, result)
		} else {
			// This is the first occurrence of an error
			logger.Debug("Changing state", "previous_state", state.State, "next_state", eval.Error)
			state.State = eval.Error
			state.StartsAt = result.EvaluatedAt
			state.setEndsAt(rule, result)
		}
	case models.OkErrState:
		resultNormal(state, rule, result, logger)
	default:
		state.State = eval.Error
		state.Error = fmt.Errorf("unsupported execution error state: %s", rule.ExecErrState)
		state.Annotations["Error"] = state.Error.Error()
	}
}

func resultNoData(state *State, rule *models.AlertRule, result eval.Result, _ log.Logger) {
	state.Error = result.Error

	if state.StartsAt.IsZero() {
		state.StartsAt = result.EvaluatedAt
	}
	state.setEndsAt(rule, result)

	switch rule.NoDataState {
	case models.Alerting:
		state.State = eval.Alerting
	case models.NoData:
		state.State = eval.NoData
	case models.OK:
		state.State = eval.Normal
	}
}

func (a *State) NeedsSending(resendDelay time.Duration) bool {
	switch a.State {
	case eval.Pending:
		// We do not send notifications for pending states
		return false
	case eval.Normal:
		// We should send a notification if the state is Normal because it was resolved
		return a.Resolved
	default:
		// We should send, and re-send notifications, each time LastSentAt is <= LastEvaluationTime + resendDelay
		nextSent := a.LastSentAt.Add(resendDelay)
		return nextSent.Before(a.LastEvaluationTime) || nextSent.Equal(a.LastEvaluationTime)
	}
}

func (a *State) Equals(b *State) bool {
	return a.AlertRuleUID == b.AlertRuleUID &&
		a.OrgID == b.OrgID &&
		a.CacheID == b.CacheID &&
		a.Labels.String() == b.Labels.String() &&
		a.State.String() == b.State.String() &&
		a.StartsAt == b.StartsAt &&
		a.EndsAt == b.EndsAt &&
		a.LastEvaluationTime == b.LastEvaluationTime &&
		data.Labels(a.Annotations).String() == data.Labels(b.Annotations).String()
}

func (a *State) TrimResults(alertRule *models.AlertRule) {
	numBuckets := int64(alertRule.For.Seconds()) / alertRule.IntervalSeconds
	if numBuckets == 0 {
		numBuckets = 10 // keep at least 10 evaluations in the event For is set to 0
	}

	if len(a.Results) < int(numBuckets) {
		return
	}
	newResults := make([]Evaluation, numBuckets)
	copy(newResults, a.Results[len(a.Results)-int(numBuckets):])
	a.Results = newResults
}

// setEndsAt sets the ending timestamp of the alert.
// The internal Alertmanager will use this time to know when it should automatically resolve the alert
// in case it hasn't received additional alerts. Under regular operations the scheduler will continue to send the
// alert with an updated EndsAt, if the alert is resolved then a last alert is sent with EndsAt = last evaluation time.
func (a *State) setEndsAt(alertRule *models.AlertRule, result eval.Result) {
	ends := ResendDelay
	if alertRule.IntervalSeconds > int64(ResendDelay.Seconds()) {
		ends = time.Second * time.Duration(alertRule.IntervalSeconds)
	}

	a.EndsAt = result.EvaluatedAt.Add(ends * 3)
}

func (a *State) GetLabels(opts ...models.LabelOption) map[string]string {
	labels := a.Labels.Copy()

	for _, opt := range opts {
		opt(labels)
	}

	return labels
}

func (a *State) GetLastEvaluationValuesForCondition() map[string]float64 {
	if len(a.Results) <= 0 {
		return nil
	}

	lastResult := a.Results[len(a.Results)-1]
	r := make(map[string]float64, len(lastResult.Values))

	for refID, value := range lastResult.Values {
		if strings.Contains(refID, lastResult.Condition) {
			if value != nil {
				r[refID] = *value
				continue
			}
			r[refID] = math.NaN()
		}
	}

	return r
}

// shouldTakeImage returns true if the state just has transitioned to alerting from another state,
// transitioned to alerting in a previous evaluation but does not have a screenshot, or has just
// been resolved.
func shouldTakeImage(state, previousState eval.State, previousImage *models.Image, resolved bool) bool {
	return resolved ||
		state == eval.Alerting && previousState != eval.Alerting ||
		state == eval.Alerting && previousImage == nil
}

// takeImage takes an image for the alert rule. It returns nil if screenshots are disabled or
// the rule is not associated with a dashboard panel.
func takeImage(ctx context.Context, s ImageCapturer, r *models.AlertRule) (*models.Image, error) {
	img, err := s.NewImage(ctx, r)
	if err != nil {
		if errors.Is(err, screenshot.ErrScreenshotsUnavailable) ||
			errors.Is(err, models.ErrNoDashboard) ||
			errors.Is(err, models.ErrNoPanel) {
			return nil, nil
		}
		return nil, err
	}
	return img, nil
}

func FormatStateAndReason(state eval.State, reason string) string {
	s := fmt.Sprintf("%v", state)
	if len(reason) > 0 {
		s += fmt.Sprintf(" (%v)", reason)
	}
	return s
}
