package pas

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/fhir"
)

// DefaultRulesFile is the rules file relative to the repository root (SPEC section 1).
const DefaultRulesFile = "spec/adjudication-rules.json"

// embeddedRules is a bundled copy of spec/adjudication-rules.json, used when no
// rules file is configured and none is found on disk.
//
//go:embed adjudication-rules.json
var embeddedRules []byte

// Rules are the deterministic adjudication rules (spec/adjudication-rules.json, SPEC section 6).
type Rules struct {
	RulesVersion                          string              `json:"rulesVersion"`
	ReviewActionCodeSystem                string              `json:"reviewActionCodeSystem"`
	Decisions                             map[string]Decision `json:"decisions"`
	ItemRules                             []ItemRule          `json:"itemRules"`
	AuthorizationNumberTemplate           string              `json:"authorizationNumberTemplate"`
	ClaimAuthorizationNumberTemplate      string              `json:"claimAuthorizationNumberTemplate"`
	AdministrationReferenceNumberTemplate string              `json:"administrationReferenceNumberTemplate"`
	ReplacedAuthorizations                map[string]string   `json:"replacedAuthorizations"`
	PreAuthPeriodDays                     int                 `json:"preAuthPeriodDays"`
	Payer                                 struct {
		ApplicationSenderCode string `json:"applicationSenderCode"`
		ReviewerNPI           string `json:"reviewerNpi"`
	} `json:"payer"`

	byID        map[string]*ItemRule
	defaultRule *ItemRule
}

// Decision is an X12 306 review action.
type Decision struct {
	Code      string `json:"code"`
	Display   string `json:"display"`
	Certified bool   `json:"certified"`
}

// ItemRule decides one Claim.item. The first matching rule wins.
type ItemRule struct {
	ID                           string           `json:"id"`
	When                         RuleWhen         `json:"when"`
	Decision                     string           `json:"decision"`
	ReviewActionReasonCode       json.RawMessage  `json:"reviewActionReasonCode,omitempty"`
	AutoFinalize                 bool             `json:"autoFinalize,omitempty"`
	FinalDecision                string           `json:"finalDecision,omitempty"`
	AddError                     *AddErrorSpec    `json:"addError,omitempty"`
	RequestAdditionalInformation *InfoRequestSpec `json:"requestAdditionalInformation,omitempty"`
	AddItem                      *AddItemSpec     `json:"addItem,omitempty"`
}

// RuleWhen is a rule condition. An empty condition matches every item.
type RuleWhen struct {
	ItemModifierExtensionInfoCancelled *bool     `json:"itemModifierExtensionInfoCancelled,omitempty"`
	ClaimCertificationTypeCode         *string   `json:"claimCertificationTypeCode,omitempty"`
	ProductOrServiceCodes              *[]string `json:"productOrServiceCodes,omitempty"`
}

func (w RuleWhen) empty() bool {
	return w.ItemModifierExtensionInfoCancelled == nil && w.ClaimCertificationTypeCode == nil && w.ProductOrServiceCodes == nil
}

// AddErrorSpec populates ClaimResponse.error for the item.
type AddErrorSpec struct {
	Code              json.RawMessage `json:"code"`
	FollowupAction    json.RawMessage `json:"followupAction"`
	ErrorElement      string          `json:"errorElement"`
	ErrorPathTemplate string          `json:"errorPathTemplate"`
}

// InfoRequestSpec asks for documentation with a CommunicationRequest and a Task.
type InfoRequestSpec struct {
	CommunicationRequestCategory json.RawMessage `json:"communicationRequestCategory"`
	AttachmentLoinc              json.RawMessage `json:"attachmentLoinc"`
	ContentModifier              json.RawMessage `json:"contentModifier"`
}

// AddItemSpec is the substitute service returned for a modified item.
type AddItemSpec struct {
	ProductOrService json.RawMessage `json:"productOrService"`
	Modifier         json.RawMessage `json:"modifier"`
	Quantity         json.Number     `json:"quantity"`
	UnitPriceUSD     json.Number     `json:"unitPriceUSD"`
	Decision         string          `json:"decision"`
}

// ParseRules decodes and checks a rules document.
func ParseRules(data []byte) (*Rules, error) {
	var r Rules
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("adjudication rules: %w", err)
	}
	if err := r.init(); err != nil {
		return nil, fmt.Errorf("adjudication rules: %w", err)
	}
	return &r, nil
}

// LoadRulesFile reads and parses a rules file.
func LoadRulesFile(path string) (*Rules, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("adjudication rules: %w", err)
	}
	return ParseRules(data)
}

// EmbeddedRules returns the bundled copy of spec/adjudication-rules.json.
func EmbeddedRules() (*Rules, error) { return ParseRules(embeddedRules) }

func (r *Rules) init() error {
	var errs []error
	for _, name := range []string{"approved", "partial", "denied", "pended", "modified", "cancelled"} {
		d, ok := r.Decisions[name]
		if !ok || d.Code == "" {
			errs = append(errs, fmt.Errorf("decision %q is missing", name))
		}
	}
	decisionOK := func(ruleID, field, name string) {
		if _, ok := r.Decisions[name]; !ok {
			errs = append(errs, fmt.Errorf("rule %s: %s %q is not a known decision", ruleID, field, name))
		}
	}
	objectOK := func(ruleID, field string, raw json.RawMessage) {
		if fragment(raw) == nil {
			errs = append(errs, fmt.Errorf("rule %s: %s must be a JSON object", ruleID, field))
		}
	}
	r.byID = map[string]*ItemRule{}
	for i := range r.ItemRules {
		rule := &r.ItemRules[i]
		if rule.ID == "" || r.byID[rule.ID] != nil {
			errs = append(errs, fmt.Errorf("rule %d: id is missing or duplicated", i))
			continue
		}
		r.byID[rule.ID] = rule
		decisionOK(rule.ID, "decision", rule.Decision)
		if rule.AutoFinalize {
			decisionOK(rule.ID, "finalDecision", rule.FinalDecision)
		}
		if len(rule.ReviewActionReasonCode) > 0 {
			objectOK(rule.ID, "reviewActionReasonCode", rule.ReviewActionReasonCode)
		}
		if a := rule.AddItem; a != nil {
			decisionOK(rule.ID, "addItem.decision", a.Decision)
			objectOK(rule.ID, "addItem.productOrService", a.ProductOrService)
			objectOK(rule.ID, "addItem.modifier", a.Modifier)
			if _, err := a.Quantity.Float64(); err != nil {
				errs = append(errs, fmt.Errorf("rule %s: addItem.quantity must be a number", rule.ID))
			}
			if _, err := a.UnitPriceUSD.Float64(); err != nil {
				errs = append(errs, fmt.Errorf("rule %s: addItem.unitPriceUSD must be a number", rule.ID))
			}
		}
		if a := rule.AddError; a != nil {
			objectOK(rule.ID, "addError.code", a.Code)
			objectOK(rule.ID, "addError.followupAction", a.FollowupAction)
		}
		if a := rule.RequestAdditionalInformation; a != nil {
			objectOK(rule.ID, "requestAdditionalInformation.communicationRequestCategory", a.CommunicationRequestCategory)
			objectOK(rule.ID, "requestAdditionalInformation.contentModifier", a.ContentModifier)
			if str(fragment(a.AttachmentLoinc), "code") == "" {
				errs = append(errs, fmt.Errorf("rule %s: requestAdditionalInformation.attachmentLoinc needs a code", rule.ID))
			}
		}
		if rule.When.empty() && r.defaultRule == nil {
			r.defaultRule = rule
		}
	}
	if r.defaultRule == nil {
		errs = append(errs, errors.New("a default rule with an empty condition is required"))
	}
	if r.ReviewActionCodeSystem == "" {
		errs = append(errs, errors.New("reviewActionCodeSystem is required"))
	}
	if !strings.Contains(r.AuthorizationNumberTemplate, "{itemSequence}") {
		errs = append(errs, errors.New("authorizationNumberTemplate must contain {itemSequence}"))
	}
	if r.ClaimAuthorizationNumberTemplate == "" {
		errs = append(errs, errors.New("claimAuthorizationNumberTemplate is required"))
	}
	if !strings.Contains(r.AdministrationReferenceNumberTemplate, "{itemSequence}") {
		errs = append(errs, errors.New("administrationReferenceNumberTemplate must contain {itemSequence}"))
	}
	if r.PreAuthPeriodDays <= 0 {
		errs = append(errs, errors.New("preAuthPeriodDays must be positive"))
	}
	if r.Payer.ApplicationSenderCode == "" || r.Payer.ReviewerNPI == "" {
		errs = append(errs, errors.New("payer.applicationSenderCode and payer.reviewerNpi are required"))
	}
	return errors.Join(errs...)
}

// rule returns the rule with id, or the default rule for unknown ids.
func (r *Rules) rule(id string) *ItemRule {
	if rule := r.byID[id]; rule != nil {
		return rule
	}
	return r.defaultRule
}

// fragment decodes a JSON object from the rules file (key order preserved).
// Each call returns a new value, so callers may place it in a response.
func fragment(raw json.RawMessage) object {
	if len(raw) == 0 {
		return nil
	}
	v, err := fhir.DecodeOrdered(raw)
	if err != nil {
		return nil
	}
	return asObj(v)
}

func fillTemplate(template, claimIdentifierValue, itemSequence string) string {
	return strings.NewReplacer("{claimIdentifierValue}", claimIdentifierValue, "{itemSequence}", itemSequence).Replace(template)
}
