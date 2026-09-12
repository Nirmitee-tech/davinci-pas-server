package pas

import (
	"strconv"
	"strings"
	"time"

	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/fhir"
)

const (
	opSubmit  = "submit"
	opInquire = "inquire"

	// ServerVersion is the software version reported in the CapabilityStatement.
	ServerVersion = "0.1.0"
)

// engine builds PAS resources from requests, rules and stored state. It holds
// no mutable state; the handler serializes access to records.
type engine struct {
	rules *Rules
	now   func() time.Time
	newID func() string
}

// itemState is an ItemResult plus, for $inquire, the stored submit item it came from.
type itemState struct {
	ItemResult
	storedItem object
	storedURL  string
}

func (e *engine) instant() string { return e.now().UTC().Format(instantLayout) }

func (e *engine) today(days int) string { return e.now().UTC().AddDate(0, 0, days).Format(dateLayout) }

func versionLabel(version string) string {
	if version == V201 {
		return "2.0.1"
	}
	return "2.2.1"
}

// metadata returns the CapabilityStatement (SPEC 3).
func (e *engine) metadata(version, base string) object {
	label := versionLabel(version)
	return obj(
		"resourceType", "CapabilityStatement",
		"id", "nirmitee-pas-server-"+version,
		"url", base+"/metadata",
		"version", ServerVersion,
		"name", "NirmiteePASServer",
		"title", "Nirmitee PAS Server (Da Vinci PAS "+label+")",
		"status", "active",
		"experimental", true,
		"date", "2026-09-12",
		"publisher", "Nirmitee",
		"description", "Da Vinci Prior Authorization Support "+label+" payer server with synthetic adjudication rules.",
		"kind", "instance",
		"software", obj("name", "Nirmitee PAS Server", "version", ServerVersion),
		"implementation", obj("description", "Nirmitee PAS Server "+label, "url", base),
		"fhirVersion", "4.0.1",
		"format", []any{"application/fhir+json", "json"},
		"rest", []any{obj(
			"mode", "server",
			"resource", []any{
				obj("type", "Claim", "operation", []any{
					obj("name", "submit", "definition", "http://hl7.org/fhir/us/davinci-pas/OperationDefinition/Claim-submit"),
					obj("name", "inquire", "definition", "http://hl7.org/fhir/us/davinci-pas/OperationDefinition/Claim-inquiry"),
				}),
				obj("type", "ClaimResponse", "interaction", []any{obj("code", "read")}),
				obj("type", "Subscription",
					"extension", []any{obj("url", backportSD+"capabilitystatement-subscriptiontopic-canonical", "valueCanonical", pasTopic)},
					"interaction", []any{obj("code", "create"), obj("code", "read"), obj("code", "update"), obj("code", "delete")}),
			},
		)},
	)
}

// validateRequest returns an OperationOutcome when a $submit or $inquire body
// cannot be processed (SPEC 4.1, 5.1), or nil.
func validateRequest(body any, op string) object {
	bundle, ok := body.(map[string]any)
	if !ok || fhir.Type(bundle) != "Bundle" {
		return outcome("error", "structure", "The request body must be a PAS Request Bundle.", "Bundle")
	}
	if str(bundle, "type") != "collection" {
		return outcome("error", "invalid", "Bundle.type must be collection.", "Bundle.type")
	}
	entries := list(bundle["entry"])
	if len(entries) == 0 {
		return outcome("error", "required", "The PAS Bundle has no entries; the first entry must be a Claim.", "Bundle.entry")
	}
	claim := requestClaim(bundle, op)
	if fhir.Type(claim) != "Claim" {
		return outcome("error", "invalid", "The first Bundle entry must be a Claim.", "Bundle.entry[0].resource")
	}
	urls := map[string]bool{}
	for _, en := range entries {
		urls[str(en, "fullUrl")] = true
	}
	for _, field := range []string{"patient", "insurer", "provider"} {
		ref := str(claim, field, "reference")
		if ref == "" || !inBundle(ref, urls) {
			return outcome("error", "invalid", "Claim."+field+" must reference a resource included in the Bundle.", "Claim."+field)
		}
	}
	if op == opSubmit && len(list(claim["item"])) == 0 {
		return outcome("error", "required", "Claim.item is required for $submit.", "Claim.item")
	}
	return nil
}

func inBundle(ref string, urls map[string]bool) bool {
	if urls[ref] {
		return true
	}
	for u := range urls {
		if u != "" && strings.HasSuffix(u, "/"+ref) {
			return true
		}
	}
	return false
}

// requestClaim is the first entry for $submit and the first Claim entry for $inquire.
func requestClaim(bundle object, op string) object {
	entries := list(bundle["entry"])
	if op == opSubmit {
		if len(entries) == 0 {
			return nil
		}
		return asObj(get(entries[0], "resource"))
	}
	for _, en := range entries {
		if r := asObj(get(en, "resource")); fhir.Type(r) == "Claim" {
			return r
		}
	}
	return nil
}

// claimKey is Claim.identifier[0] system|value.
func claimKey(claim object) string {
	ids := list(claim["identifier"])
	if len(ids) == 0 {
		return "|"
	}
	return str(ids[0], "system") + "|" + str(ids[0], "value")
}

// itemRule returns the first matching rule for a Claim.item (SPEC 6).
func (e *engine) itemRule(claim, item object) *ItemRule {
	for i := range e.rules.ItemRules {
		rule := &e.rules.ItemRules[i]
		w := rule.When
		if w.ItemModifierExtensionInfoCancelled != nil && *w.ItemModifierExtensionInfoCancelled {
			for _, m := range list(item["modifierExtension"]) {
				if b, ok := get(m, "valueBoolean").(bool); ok && b && str(m, "url") == pasSD+"modifierextension-infoCancelled" {
					return rule
				}
			}
			continue
		}
		if w.ClaimCertificationTypeCode != nil {
			if firstCode(extValue(claim, pasSD+"extension-certificationType", "valueCodeableConcept")) == *w.ClaimCertificationTypeCode {
				return rule
			}
			continue
		}
		if w.ProductOrServiceCodes != nil {
			for _, c := range fhir.Codings(item["productOrService"]) {
				for _, want := range *w.ProductOrServiceCodes {
					if str(c, "code") == want {
						return rule
					}
				}
			}
			continue
		}
		if w.empty() {
			return rule
		}
	}
	return e.rules.defaultRule
}

// claimLevel derives the claim level decision from the item decisions (SPEC 6).
func claimLevel(decisions []string) string {
	var live []string
	for _, d := range decisions {
		if d != "cancelled" {
			live = append(live, d)
		}
	}
	if len(live) == 0 {
		return "cancelled"
	}
	allDenied, anyDenied, anyCertified, anyModified := true, false, false, false
	for _, d := range live {
		switch d {
		case "pended":
			return "pended"
		case "denied":
			anyDenied = true
		case "approved", "partial":
			anyCertified = true
		case "modified":
			anyCertified, anyModified = true, true
		}
		if d != "denied" {
			allDenied = false
		}
	}
	switch {
	case allDenied:
		return "denied"
	case anyDenied && anyCertified:
		return "partial"
	case anyModified:
		return "modified"
	}
	return "approved"
}

// submit adjudicates a $submit Bundle. It returns the HTTP status, the body and,
// on success, the claim record to store (SPEC 4).
func (e *engine) submit(version, base string, body any) (int, object, *ClaimRecord) {
	if oo := validateRequest(body, opSubmit); oo != nil {
		return 400, oo, nil
	}
	bundle := asObj(body)
	first := asObj(list(bundle["entry"])[0])
	claim := asObj(first["resource"])
	var states []itemState
	var results []ItemResult
	for idx, it := range list(claim["item"]) {
		item := asObj(it)
		rule := e.itemRule(claim, item)
		res := ItemResult{Index: idx, Sequence: item["sequence"], RuleID: rule.ID, Decision: rule.Decision}
		results = append(results, res)
		states = append(states, itemState{ItemResult: res})
	}
	rec := &ClaimRecord{
		Version: version,
		Key:     claimKey(claim),
		Base:    base,
		FullURL: str(first, "fullUrl"),
		Claim:   claim,
		Bundle:  bundle,
		Results: results,
	}
	rec.LastResponse = e.responseBundle(opSubmit, version, base, claim, rec.FullURL, states, []object{bundle})
	return 200, rec.LastResponse, rec
}

// inquire answers a $inquire Bundle from the stored record, adjudicating
// unknown items statelessly (SPEC 5).
func (e *engine) inquire(version, base string, body any, lookup func(key string) *ClaimRecord) (int, object) {
	if oo := validateRequest(body, opInquire); oo != nil {
		return 400, oo
	}
	bundle := asObj(body)
	claim := requestClaim(bundle, opInquire)
	stored := lookup(claimKey(claim))
	var states []itemState
	for idx, it := range list(claim["item"]) {
		item := asObj(it)
		var found *ItemResult
		if stored != nil {
			for i := range stored.Results {
				if seqKey(stored.Results[i].Sequence) == seqKey(item["sequence"]) {
					found = &stored.Results[i]
					break
				}
			}
		}
		if found != nil {
			st := itemState{ItemResult: *found, storedURL: stored.FullURL}
			st.Index = idx
			if storedItems := list(stored.Claim["item"]); found.Index >= 0 && found.Index < len(storedItems) {
				st.storedItem = asObj(storedItems[found.Index])
			}
			states = append(states, st)
			continue
		}
		rule := e.itemRule(claim, item)
		states = append(states, itemState{ItemResult: ItemResult{Index: idx, Sequence: item["sequence"], RuleID: rule.ID, Decision: rule.Decision}})
	}
	claimURL := ""
	sources := []object{bundle}
	if stored != nil {
		claimURL = stored.FullURL
		sources = append(sources, stored.Bundle)
	}
	resp := e.responseBundle(opInquire, version, base, claim, claimURL, states, sources)
	if version == V221 {
		return 200, obj("resourceType", "Parameters", "parameter", []any{obj("name", "return", "resource", resp)})
	}
	return 200, resp
}

// autoFinalizable reports whether a record has pended items that finalize on their own.
func (e *engine) autoFinalizable(rec *ClaimRecord) bool {
	for _, r := range rec.Results {
		if r.Decision == "pended" && e.rules.rule(r.RuleID).AutoFinalize {
			return true
		}
	}
	return false
}

// finalize flips auto-finalizable pended items to their final decision and
// rebuilds the stored response. It is idempotent (SPEC 7.3).
func (e *engine) finalize(rec *ClaimRecord) (object, bool) {
	changed := false
	for i := range rec.Results {
		r := &rec.Results[i]
		if rule := e.rules.rule(r.RuleID); r.Decision == "pended" && rule.AutoFinalize {
			r.Decision = rule.FinalDecision
			r.Finalized = true
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	states := make([]itemState, len(rec.Results))
	for i, r := range rec.Results {
		states[i] = itemState{ItemResult: r}
	}
	rec.LastResponse = e.responseBundle(opSubmit, rec.Version, rec.Base, rec.Claim, rec.FullURL, states, []object{rec.Bundle})
	return rec.LastResponse, true
}

func codeable(system, code string) object {
	return obj("coding", []any{obj("system", system, "code", code)})
}

func adjudicationCategory() object {
	return codeable("http://terminology.hl7.org/CodeSystem/adjudication", "submitted")
}

type pendedLine struct {
	item object
	rule *ItemRule
}

// responseBundle builds a PAS Response Bundle (submit) or Inquiry Response
// Bundle (inquire) following SPEC 4.3 to 4.7 and 5.2.
func (e *engine) responseBundle(op, version, base string, claim object, claimURL string, results []itemState, sources []object) object {
	rules := e.rules
	v221 := version == V221

	entriesByURL := map[string]object{}
	for _, b := range sources {
		for _, en := range list(b["entry"]) {
			if m := asObj(en); m != nil {
				if _, seen := entriesByURL[str(m, "fullUrl")]; !seen {
					entriesByURL[str(m, "fullUrl")] = m
				}
			}
		}
	}
	identValue := "UNKNOWN"
	if ids := list(claim["identifier"]); len(ids) > 0 {
		if v, ok := get(ids[0], "value").(string); ok {
			identValue = v
		}
	}
	decisions := make([]string, len(results))
	for i, r := range results {
		decisions[i] = r.Decision
	}
	claimDecision := claimLevel(decisions)

	crID := e.newID()
	crURL := "urn:uuid:" + crID
	generated := map[string]object{}

	// resolve maps a reference to the fullUrl of a known entry, trying the
	// base of the referencing entry for relative references.
	resolve := func(ref, context string) string {
		if _, ok := entriesByURL[ref]; ok {
			return ref
		}
		if _, ok := generated[ref]; ok {
			return ref
		}
		if context != "" && strings.Contains(ref, "/") && !strings.HasPrefix(ref, "urn:") {
			cand := ref
			if b := extractBaseURL(context); b != "" {
				cand = b + "/" + ref
			}
			if _, ok := entriesByURL[cand]; ok {
				return cand
			}
		}
		return ref
	}
	entryResource := func(u string) object { return asObj(get(entriesByURL[u], "resource")) }

	careTeam := map[string]object{}
	var overall object
	for _, ct := range list(claim["careTeam"]) {
		m := asObj(ct)
		if m == nil {
			continue
		}
		careTeam[seqKey(m["sequence"])] = m
		if b, ok := extValue(m, pasSD+"extension-careTeamClaimScope", "valueBoolean").(bool); ok && b && overall == nil {
			overall = m
		}
	}
	claimProvider := resolve(str(claim, "provider", "reference"), claimURL)
	rolePractitioner := func(roleURL string) string {
		role := entryResource(roleURL)
		if fhir.Type(role) != "PractitionerRole" {
			return ""
		}
		return resolve(str(role, "practitioner", "reference"), roleURL)
	}
	roleLocation := func(roleURL string) string {
		locs := list(entryResource(roleURL)["location"])
		if len(locs) == 0 {
			return ""
		}
		return resolve(str(locs[0], "reference"), roleURL)
	}

	var admission, discharge object
	for _, s := range list(claim["supportingInfo"]) {
		m := asObj(s)
		switch firstCode(m["category"]) {
		case "admissionDates":
			if admission == nil {
				admission = m
			}
		case "dischargeDates":
			if discharge == nil {
				discharge = m
			}
		}
	}
	diagnoses := map[string]object{}
	for _, d := range list(claim["diagnosis"]) {
		if m := asObj(d); m != nil {
			diagnoses[seqKey(m["sequence"])] = m
		}
	}
	preAuthPeriod := func() object { return obj("start", e.today(0), "end", e.today(rules.PreAuthPeriodDays)) }
	adjudication := func(decision, number string, reason object) object {
		d := rules.Decisions[decision]
		var sub []any
		if number != "" {
			sub = append(sub, obj("url", "number", "valueString", number))
		}
		sub = append(sub, obj("url", pasSD+"extension-reviewActionCode", "valueCodeableConcept",
			obj("coding", []any{obj("system", rules.ReviewActionCodeSystem, "code", d.Code, "display", d.Display)})))
		if reason != nil {
			sub = append(sub, obj("url", "reasonCode", "valueCodeableConcept", obj("coding", []any{reason})))
		}
		return obj("extension", []any{obj("url", pasSD+"extension-reviewAction", "extension", sub)}, "category", adjudicationCategory())
	}
	authNumber := func(seq string) string { return fillTemplate(rules.AuthorizationNumberTemplate, identValue, seq) }
	admissionExt := func() object {
		if admission == nil {
			return nil
		}
		if p := admission["timingPeriod"]; present(p) {
			return obj("url", pasSD+"extension-admissionDates", "valuePeriod", clone(p))
		}
		if d, ok := admission["timingDate"].(string); ok && d != "" {
			return obj("url", pasSD+"extension-admissionDates", "valueDate", d)
		}
		return nil
	}
	dischargeExt := func() object {
		if d, ok := discharge["timingDate"].(string); ok && d != "" {
			return obj("url", pasSD+"extension-dischargeDate", "valueDate", d)
		}
		return nil
	}

	var items, addItems, errs []any
	var commRequest, taskLine *pendedLine
	claimItems := list(claim["item"])
	for _, r := range results {
		if r.Index < 0 || r.Index >= len(claimItems) {
			continue
		}
		item := asObj(claimItems[r.Index])
		seq := item["sequence"]
		seqS := seqText(seq)
		dec := r.Decision
		certified := rules.Decisions[dec].Certified
		rule := rules.rule(r.RuleID)

		var ext []any
		trace := extValue(item, pasSD+"extension-itemTraceNumber", "valueIdentifier")
		if !present(trace) {
			trace = obj("system", payerSID+"item-trace", "value", "NSHP-"+identValue+"-"+seqS)
		}
		ext = append(ext, obj("url", pasSD+"extension-itemTraceNumber", "valueIdentifier", clone(trace)))
		if certified {
			ext = append(ext,
				obj("url", pasSD+"extension-itemPreAuthIssueDate", "valueDate", e.today(0)),
				obj("url", pasSD+"extension-itemPreAuthPeriod", "valuePeriod", preAuthPeriod()))
		}
		prev, _ := extValue(item, pasSD+"extension-authorizationNumber", "valueString").(string)
		currentNumber := ""
		if certified {
			currentNumber = authNumber(seqS)
		}
		if prev != "" {
			if replacement, ok := rules.ReplacedAuthorizations[prev]; ok && op == opInquire {
				ext = append(ext, obj("url", pasSD+"extension-authorizationNumber", "valueString", replacement))
				if certified {
					currentNumber = replacement
				}
			} else {
				ext = append(ext, obj("url", pasSD+"extension-authorizationNumber", "valueString", prev))
			}
		}
		admRef, _ := extValue(item, pasSD+"extension-administrationReferenceNumber", "valueString").(string)
		if admRef == "" {
			admRef = fillTemplate(rules.AdministrationReferenceNumberTemplate, identValue, seqS)
		}
		ext = append(ext, obj("url", pasSD+"extension-administrationReferenceNumber", "valueString", admRef))
		if sp := item["servicedPeriod"]; present(sp) {
			ext = append(ext, obj("url", pasSD+"extension-itemRequestedServiceDate", "valuePeriod", clone(sp)))
		} else if sd := item["servicedDate"]; present(sd) {
			ext = append(ext, obj("url", pasSD+"extension-itemRequestedServiceDate", "valueDateTime", sd))
		}
		providerRef := ""
		for _, cts := range list(item["careTeamSequence"]) {
			ct := careTeam[seqKey(cts)]
			if b, ok := extValue(ct, pasSD+"extension-careTeamClaimScope", "valueBoolean").(bool); ct != nil && ok && !b {
				providerRef = resolve(str(ct, "provider", "reference"), claimURL)
			}
		}
		if providerRef == "" && overall != nil {
			providerRef = resolve(str(overall, "provider", "reference"), claimURL)
		}
		if providerRef == "" {
			providerRef = claimProvider
		}
		if certified {
			ext = append(ext, obj("url", pasSD+"extension-itemAuthorizedProvider", "extension",
				[]any{obj("url", "provider", "valueReference", obj("reference", providerRef))}))
		}
		spec := rule.AddItem
		modified := dec == "modified" && spec != nil
		if modified {
			quantity := obj("value", spec.Quantity, "unit", "Units")
			if v221 {
				set(quantity, "system", x12+"673")
				set(quantity, "code", "FL")
			}
			ext = append(ext, obj("url", pasSD+"extension-itemAuthorizedDetail", "extension", []any{
				obj("url", "productOrServiceCode", "valueCodeableConcept", obj("coding", []any{fragment(spec.ProductOrService)})),
				obj("url", "modifier", "valueCodeableConcept", obj("coding", []any{fragment(spec.Modifier)})),
				obj("url", "unitPrice", "valueMoney", obj("value", spec.UnitPriceUSD, "currency", "USD")),
				obj("url", "quantity", "valueQuantity", quantity),
			}))
		}
		if v221 {
			if x := admissionExt(); x != nil {
				ext = append(ext, x)
			}
			if x := dischargeExt(); x != nil {
				ext = append(ext, x)
			}
		}
		for _, dseq := range list(item["diagnosisSequence"]) {
			if diag := diagnoses[seqKey(dseq)]; diag != nil && present(diag["diagnosisCodeableConcept"]) {
				ext = append(ext, obj("url", pasSD+"extension-communicatedDiagnosis", "valueCodeableConcept", clone(diag["diagnosisCodeableConcept"])))
				break
			}
		}
		items = append(items, obj("extension", ext, "itemSequence", clone(seq), "noteNumber", []any{1},
			"adjudication", []any{adjudication(dec, currentNumber, fragment(rule.ReviewActionReasonCode))}))

		if modified {
			practitionerRef := rolePractitioner(providerRef)
			locationRef := roleLocation(providerRef)
			authForAdd := prev
			if authForAdd == "" {
				authForAdd = authNumber(seqS)
			}
			ai := obj("itemSequence", []any{clone(seq)})
			if v221 {
				ae := []any{
					obj("url", pasSD+"extension-itemTraceNumber", "valueIdentifier", clone(trace)),
					obj("url", pasSD+"extension-itemPreAuthIssueDate", "valueDate", e.today(0)),
					obj("url", pasSD+"extension-itemPreAuthPeriod", "valuePeriod", preAuthPeriod()),
					obj("url", pasSD+"extension-authorizationNumber", "valueString", authForAdd),
					obj("url", pasSD+"extension-administrationReferenceNumber", "valueString", admRef),
					obj("url", pasSD+"extension-epsdtIndicator", "valueBoolean", false),
					obj("url", pasSD+"extension-nursingHomeResidentialStatus", "valueCodeableConcept", codeable(x12+"1345", "2")),
					obj("url", pasSD+"extension-nursingHomeLevelOfCare", "valueCodeableConcept", codeable(x12+"1337", "2")),
					obj("url", pasSD+"extension-productOrServiceCodeEnd", "valueCodeableConcept", obj("coding", []any{fragment(spec.ProductOrService)})),
					obj("url", pasSD+"extension-revenueCode", "valueCodeableConcept", codeable("https://www.nubc.org/CodeSystem/RevenueCodes", "0292")),
					obj("url", pasSD+"extension-revenueUnitRateLimit", "valueDecimal", spec.UnitPriceUSD),
				}
				sourceItem := r.storedItem
				if sourceItem == nil {
					sourceItem = item
				}
				if requested := asObj(extValue(sourceItem, pasSD+"extension-requestedService", "valueReference")); present(requested) {
					context := r.storedURL
					if context == "" {
						context = claimURL
					}
					ae = append(ae, obj("url", pasSD+"extension-requestedService", "valueReference",
						obj("reference", resolve(str(requested, "reference"), context))))
				}
				for _, u := range []string{"extension-serviceItemRequestType", "extension-certificationType"} {
					if v := extValue(item, pasSD+u, "valueCodeableConcept"); present(v) {
						ae = append(ae, obj("url", pasSD+u, "valueCodeableConcept", clone(v)))
					}
				}
				if x := admissionExt(); x != nil {
					ae = append(ae, x)
				}
				if x := dischargeExt(); x != nil {
					ae = append(ae, x)
				}
				set(ai, "extension", ae)
				if practitionerRef != "" {
					set(ai, "provider", []any{obj(
						"extension", []any{obj("url", pasSD+"extension-authorizedProviderType", "valueCodeableConcept", codeable(x12+"98", "SJ"))},
						"reference", practitionerRef)})
				}
			}
			set(ai, "productOrService", obj("coding", []any{fragment(spec.ProductOrService)}))
			set(ai, "modifier", []any{obj("coding", []any{fragment(spec.Modifier)})})
			set(ai, "servicedPeriod", preAuthPeriod())
			if v221 && locationRef != "" {
				set(ai, "locationReference", obj("reference", locationRef))
			}
			if v221 {
				set(ai, "quantity", obj("value", spec.Quantity, "unit", "Units", "system", x12+"673", "code", "FL"))
			} else {
				set(ai, "quantity", obj("value", spec.Quantity))
			}
			set(ai, "unitPrice", obj("value", spec.UnitPriceUSD, "currency", "USD"))
			set(ai, "adjudication", []any{adjudication(spec.Decision, authForAdd, nil)})
			addItems = append(addItems, ai)
		}

		if ae := rule.AddError; ae != nil {
			var ee []any
			if v221 {
				ee = append(ee, obj("url", pasSD+"extension-errorElement", "extension", []any{
					obj("url", "processNote", "valuePositiveInt", 1),
					obj("url", "error", "valueString", ae.ErrorElement),
				}))
			} else {
				ee = append(ee, obj("url", pasSD+"extension-errorElement", "valueString", ae.ErrorElement))
			}
			ee = append(ee,
				obj("url", pasSD+"extension-errorPath", "valueString", strings.ReplaceAll(ae.ErrorPathTemplate, "{itemIndex}", strconv.Itoa(r.Index))),
				obj("url", pasSD+"extension-errorFollowupAction", "valueCodeableConcept", obj("coding", []any{fragment(ae.FollowupAction)})))
			errs = append(errs, obj("extension", ee, "itemSequence", clone(seq), "code", obj("coding", []any{fragment(ae.Code)})))
		}
		if dec == "pended" && rule.RequestAdditionalInformation != nil {
			line := &pendedLine{item: item, rule: rule}
			if op == opSubmit {
				commRequest = line
			}
			taskLine = line
		}
	}

	patientRef := resolve(str(claim, "patient", "reference"), claimURL)
	insurerRef := resolve(str(claim, "insurer", "reference"), claimURL)
	profile := "profile-claimresponse"
	if op == opInquire {
		profile = "profile-claiminquiryresponse"
	}
	cr := obj("resourceType", "ClaimResponse", "id", crID, "meta", obj("profile", []any{pasSD + profile}))
	if v221 {
		sender := map[string]string{}
		for _, x := range list(extValue(claim, pasSD+"extension-TransmissionIdentifiers", "extension")) {
			s, _ := get(x, "valueString").(string)
			sender[str(x, "url")] = s
		}
		appSender := sender["applicationReceiverCode"]
		if appSender == "" {
			appSender = rules.Payer.ApplicationSenderCode
		}
		appReceiver := sender["applicationSenderCode"]
		if appReceiver == "" {
			appReceiver = "REQUESTOR"
		}
		ti := []any{
			obj("url", "applicationSenderCode", "valueString", appSender),
			obj("url", "applicationReceiverCode", "valueString", appReceiver),
		}
		if v := sender["interchangeReceiverID"]; v != "" {
			ti = append(ti, obj("url", "interchangeSenderID", "valueString", v))
		}
		if v := sender["interchangeSenderID"]; v != "" {
			ti = append(ti, obj("url", "interchangeReceiverID", "valueString", v))
		}
		humanReviewed := false
		for _, r := range results {
			if r.Decision == "denied" || r.Decision == "modified" || r.Finalized {
				humanReviewed = true
			}
		}
		rootProvider := claimProvider
		if overall != nil {
			rootProvider = resolve(str(overall, "provider", "reference"), claimURL)
		}
		set(cr, "extension", []any{
			obj("url", pasSD+"extension-TransmissionIdentifiers", "extension", ti),
			obj("url", pasSD+"extension-claimResponseReviewer", "extension", []any{
				obj("url", "wasHumanReviewedFlag", "valueBoolean", humanReviewed),
				obj("url", "reviewerNPI", "valueIdentifier", obj("system", npiSystem, "value", rules.Payer.ReviewerNPI)),
			}),
			obj("url", pasSD+"extension-itemAuthorizedProvider", "extension",
				[]any{obj("url", "provider", "valueReference", obj("reference", rootProvider))}),
		})
	}
	if v := claim["identifier"]; v != nil {
		set(cr, "identifier", clone(v))
	}
	set(cr, "status", "active")
	if v := claim["type"]; v != nil {
		set(cr, "type", clone(v))
	}
	set(cr, "use", "preauthorization")
	set(cr, "patient", obj("reference", patientRef))
	set(cr, "created", e.instant())
	set(cr, "insurer", obj("reference", insurerRef))
	set(cr, "requestor", obj("reference", claimProvider))
	if strings.HasPrefix(claimURL, "http://") || strings.HasPrefix(claimURL, "https://") {
		set(cr, "request", obj("reference", claimURL))
	} else {
		set(cr, "request", obj("extension", []any{obj("url", darURL, "valueCode", "unknown")}))
	}
	switch {
	case len(errs) > 0:
		set(cr, "outcome", "partial")
	case claimDecision == "pended" && !v221:
		set(cr, "outcome", "queued")
	default:
		set(cr, "outcome", "complete")
	}
	set(cr, "preAuthPeriod", preAuthPeriod())

	requestorNPI := "UNKNOWN"
	for _, id := range list(entryResource(claimProvider)["identifier"]) {
		if str(id, "system") == npiSystem {
			requestorNPI = str(id, "value")
			break
		}
	}

	if commRequest != nil {
		spec := commRequest.rule.RequestAdditionalInformation
		item := commRequest.item
		seq := item["sequence"]
		crqID := e.newID()
		crqURL := "urn:uuid:" + crqID
		var payloadExt []any
		if !v221 {
			payloadExt = append(payloadExt, obj("url", pasSD+"extension-serviceLineNumber", "valuePositiveInt", clone(seq)))
		}
		if dseqs := list(item["diagnosisSequence"]); len(dseqs) > 0 {
			if diag := diagnoses[seqKey(dseqs[0])]; diag != nil && present(diag["diagnosisCodeableConcept"]) {
				payloadExt = append(payloadExt, obj("url", pasSD+"extension-communicatedDiagnosis", "valueCodeableConcept", clone(diag["diagnosisCodeableConcept"])))
			}
		}
		payloadExt = append(payloadExt, obj("url", pasSD+"extension-contentModifier", "valueCodeableConcept", obj("coding", []any{fragment(spec.ContentModifier)})))
		crq := obj("resourceType", "CommunicationRequest", "id", crqID, "meta", obj("profile", []any{pasSD + "profile-communicationrequest"}))
		if v221 {
			set(crq, "extension", []any{obj("url", pasSD+"extension-serviceLineNumber", "valuePositiveInt", clone(seq))})
		}
		set(crq, "identifier", []any{obj("system", payerSID+"communication-request-id", "value", "CR-"+identValue+"-"+seqText(seq))})
		set(crq, "status", "active")
		set(crq, "category", []any{obj("coding", []any{fragment(spec.CommunicationRequestCategory)})})
		set(crq, "medium", []any{codeable("http://terminology.hl7.org/CodeSystem/v3-ParticipationMode", "ELECTRONIC")})
		set(crq, "subject", obj("reference", patientRef))
		set(crq, "payload", []any{obj("extension", payloadExt, "contentString", str(fragment(spec.AttachmentLoinc), "code"))})
		set(crq, "authoredOn", e.instant())
		set(crq, "requester", obj("reference", insurerRef))
		set(crq, "recipient", []any{obj("reference", claimProvider)})
		set(crq, "sender", obj("reference", claimProvider))
		generated[crqURL] = obj("fullUrl", crqURL, "resource", crq)
		set(cr, "communicationRequest", []any{obj("reference", crqURL)})
	}
	if len(items) > 0 {
		set(cr, "item", items)
	}
	if len(addItems) > 0 {
		set(cr, "addItem", addItems)
	}
	// Header adjudication: extension-reviewActionCode is only allowed on item and
	// addItem adjudications, so the claim level decision is carried by the items
	// and the processNote; this reviewAction holds the claim number and flag (SPEC 4.4).
	var headerReview []any
	if rules.Decisions[claimDecision].Certified {
		headerReview = append(headerReview, obj("url", "number", "valueString", fillTemplate(rules.ClaimAuthorizationNumberTemplate, identValue, "")))
	}
	headerReview = append(headerReview, obj("url", "secondSurgicalOpinionFlag", "valueBoolean", false))
	set(cr, "adjudication", []any{obj(
		"extension", []any{obj("url", pasSD+"extension-reviewAction", "extension", headerReview)},
		"category", adjudicationCategory())})
	d := rules.Decisions[claimDecision]
	set(cr, "processNote", []any{obj("number", 1, "type", "display",
		"text", "Synthetic adjudication: claim decision "+d.Code+" "+d.Display+".")})
	if len(errs) > 0 {
		set(cr, "error", errs)
	}

	var task object
	if taskLine != nil {
		spec := taskLine.rule.RequestAdditionalInformation
		item := taskLine.item
		seq := item["sequence"]
		var reasonReference object
		if strings.HasPrefix(claimURL, "http") {
			reasonReference = obj("reference", claimURL)
		} else {
			ident := obj()
			if ids := list(claim["identifier"]); len(ids) > 0 {
				first := asObj(ids[0])
				for _, k := range orderedKeys(first) {
					if k == "system" || k == "value" {
						set(ident, k, first[k])
					}
				}
			}
			reasonReference = obj("identifier", ident)
		}
		lineURL, lineKey := pasSD+"extension-paLineNumber", "valueInteger"
		if v221 {
			lineURL, lineKey = pasSD+"extension-serviceLineNumber", "valuePositiveInt"
		}
		lineExt := func() object { return obj("url", lineURL, lineKey, clone(seq)) }
		attachmentExt := []any{lineExt()}
		if v221 {
			attachmentExt = append(attachmentExt, obj("url", pasSD+"extension-contentModifier", "valueCodeableConcept", obj("coding", []any{fragment(spec.ContentModifier)})))
		}
		inputs := []any{
			obj("type", codeable(pasTemp, "payer-url"), "valueUrl", base),
			obj("extension", attachmentExt,
				"type", codeable(pasTemp, "attachments-needed"),
				"valueCodeableConcept", codeable(loincSystem, str(fragment(spec.AttachmentLoinc), "code"))),
		}
		if v221 {
			inputs = append(inputs, obj("extension", []any{lineExt()},
				"type", codeable(pasTemp, "questionnaire-context"),
				"valueString", base+"/Questionnaire/pas-home-oxygen-documentation"))
		}
		task = obj(
			"resourceType", "Task",
			"id", e.newID(),
			"meta", obj("profile", []any{pasSD + "profile-task"}),
			"identifier", []any{obj("system", payerSID+"task-id", "value", "TASK-"+identValue+"-"+seqText(seq))},
			"status", "requested",
			"statusReason", obj("text", "Additional documentation is required to complete the prior authorization review"),
			"intent", "order",
			"code", codeable(pasTemp, "attachment-request-code"),
			"for", obj("reference", patientRef),
			"authoredOn", e.instant(),
			"requester", obj("identifier", obj("system", payerSID+"payer-id", "value", "NSHP01")),
			"owner", obj("identifier", obj("system", npiSystem, "value", requestorNPI)),
			"reasonCode", codeable(pasTemp, "priorAuthorization"),
			"reasonReference", reasonReference,
			"restriction", obj("period", obj("start", e.today(0), "end", e.today(30))),
			"input", inputs,
		)
	}

	// Bundle closure: the ClaimResponse first, then every resource reachable from
	// it (ClaimResponse.request excluded, Claims never echoed), then the Task.
	out := []any{obj("fullUrl", crURL, "resource", cr)}
	included := map[string]bool{crURL: true}
	var include func(u string)
	include = func(u string) {
		if u == "" || included[u] {
			return
		}
		if g, ok := generated[u]; ok {
			included[u] = true
			out = append(out, g)
			for _, ref := range iterReferences(g["resource"]) {
				include(resolve(ref, u))
			}
			return
		}
		res := entryResource(u)
		if res == nil || fhir.Type(res) == "Claim" {
			return
		}
		included[u] = true
		out = append(out, obj("fullUrl", u, "resource", clone(res)))
		for _, ref := range iterReferences(res) {
			include(resolve(ref, u))
		}
	}
	for _, ref := range iterReferences(cr) {
		include(resolve(ref, claimURL))
	}
	if task != nil {
		include(patientRef)
		out = append(out, obj("fullUrl", "urn:uuid:"+fhir.ID(task), "resource", task))
	}
	bundleProfile := "profile-pas-response-bundle"
	if op == opInquire {
		bundleProfile = "profile-pas-inquiry-response-bundle"
	}
	return obj(
		"resourceType", "Bundle",
		"id", e.newID(),
		"meta", obj("profile", []any{pasSD + bundleProfile}),
		"identifier", obj("system", "urn:ietf:rfc:3986", "value", "urn:uuid:"+e.newID()),
		"type", "collection",
		"timestamp", e.instant(),
		"entry", out,
	)
}

// iterReferences lists Reference.reference values in document order, skipping
// ClaimResponse.request.
func iterReferences(v any) []string {
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case []any:
			for _, x := range t {
				walk(x)
			}
		case map[string]any:
			if ref, ok := t["reference"].(string); ok {
				out = append(out, ref)
				return
			}
			isClaimResponse := fhir.Type(t) == "ClaimResponse"
			for _, k := range orderedKeys(t) {
				if k == "request" && isClaimResponse {
					continue
				}
				walk(t[k])
			}
		}
	}
	walk(v)
	return out
}

// Extensions the IG marks must support outside their declared context. The
// kit suppresses those validator errors for $submit and $inquire responses but
// not inside a full-resource notification, so the notification copy omits
// them (SPEC 7.4).
var notificationOmit = map[string][]string{
	"root": {pasSD + "extension-itemAuthorizedProvider"},
	"item": {pasSD + "extension-communicatedDiagnosis"},
	"addItem": {
		pasSD + "extension-productOrServiceCodeEnd", pasSD + "extension-revenueUnitRateLimit",
		pasSD + "extension-serviceItemRequestType", pasSD + "extension-certificationType",
	},
}

func withoutExtensions(exts []any, urls []string) []any {
	out := make([]any, 0, len(exts))
	for _, x := range exts {
		drop := false
		for _, u := range urls {
			if str(x, "url") == u {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, x)
		}
	}
	return out
}

// notificationCopy returns a deep copy of a response Bundle for a full-resource notification.
func notificationCopy(bundle object) object {
	out := asObj(clone(bundle))
	for _, en := range list(out["entry"]) {
		cr := asObj(get(en, "resource"))
		if fhir.Type(cr) != "ClaimResponse" {
			continue
		}
		if kept := withoutExtensions(list(cr["extension"]), notificationOmit["root"]); len(kept) > 0 {
			cr["extension"] = kept
		} else {
			delete(cr, "extension")
		}
		for _, key := range []string{"item", "addItem"} {
			for _, it := range list(cr[key]) {
				m := asObj(it)
				if m == nil || m["extension"] == nil {
					continue
				}
				if kept := withoutExtensions(list(m["extension"]), notificationOmit[key]); len(kept) > 0 {
					m["extension"] = kept
				} else {
					delete(m, "extension")
				}
			}
		}
	}
	return out
}

// firstClaimResponse returns the first ClaimResponse entry of a Bundle.
func firstClaimResponse(bundle object) object {
	for _, en := range list(bundle["entry"]) {
		if r := asObj(get(en, "resource")); fhir.Type(r) == "ClaimResponse" {
			return r
		}
	}
	return nil
}

// summarizeResponse describes the decisions in a response for logs, e.g. "PA-2026-0001 1=A1".
func summarizeResponse(body object) string {
	bundle := body
	if fhir.Type(body) == "Parameters" {
		for _, p := range list(body["parameter"]) {
			if r := asObj(get(p, "resource")); fhir.Type(r) == "Bundle" {
				bundle = r
				break
			}
		}
	}
	cr := firstClaimResponse(bundle)
	if cr == nil {
		return ""
	}
	parts := []string{}
	if ids := list(cr["identifier"]); len(ids) > 0 {
		parts = append(parts, str(ids[0], "value"))
	}
	for _, it := range list(cr["item"]) {
		code := ""
		for _, adj := range list(get(it, "adjudication")) {
			for _, x := range list(get(adj, "extension")) {
				for _, s := range list(get(x, "extension")) {
					if str(s, "url") == pasSD+"extension-reviewActionCode" {
						code = firstCode(get(s, "valueCodeableConcept"))
					}
				}
			}
		}
		parts = append(parts, seqText(get(it, "itemSequence"))+"="+code)
	}
	return strings.Join(parts, " ")
}
