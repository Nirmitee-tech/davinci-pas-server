using System.Globalization;
using System.Text.Json.Nodes;
using Nirmitee.DaVinci.Pas.Fhir;
using Nirmitee.DaVinci.Pas.Rules;
using Nirmitee.DaVinci.Pas.Storage;

namespace Nirmitee.DaVinci.Pas.Claims;

/// <summary>
/// Builds PAS Response and Inquiry Response Bundles (SPEC sections 4.3 to 4.7 and 5.2). The construction order of
/// every element follows the reference model so the output has the same shape and key order as the examples.
/// </summary>
internal sealed class ResponseBuilder(AdjudicationRules rules, string version, string fhirBase, DateTimeOffset now)
{
    private readonly bool _v221 = version == PasVersions.V221;
    private readonly string _base = fhirBase.TrimEnd('/');

    private string Instant => FhirJson.Instant(now);

    private string Today(int plusDays = 0) => FhirJson.Date(now, plusDays);

    private static string Sd(string name) => PasUrls.Sd(name);

    /// <param name="operation">submit or inquire.</param>
    /// <param name="claim">The Claim the response answers (the inquiry Claim for $inquire).</param>
    /// <param name="claimUrl">The submit Claim fullUrl (for $inquire the stored one, or null).</param>
    /// <param name="results">One result per Claim.item, in item order.</param>
    /// <param name="sources">Bundles whose entries may be echoed; earlier Bundles win on duplicate fullUrls.</param>
    public JsonObject Build(string operation, JsonObject claim, string? claimUrl, IReadOnlyList<ItemResult> results, IReadOnlyList<JsonObject> sources)
    {
        var entriesByUrl = new Dictionary<string, JsonObject>(StringComparer.Ordinal);
        foreach (var source in sources)
        {
            foreach (var entry in J.Objs(source, "entry"))
            {
                if (J.Str(entry, "fullUrl") is { } url)
                {
                    entriesByUrl.TryAdd(url, entry);
                }
            }
        }

        var identValue = IdentifierValue(claim);
        var decisions = results.Select(r => r.Decision).ToList();
        var claimDecision = AdjudicationRules.ClaimLevel(decisions);

        var crId = FhirJson.NewId();
        var crUrl = "urn:uuid:" + crId;
        var generated = new OrderedDictionary<string, JsonObject>(StringComparer.Ordinal);

        string Resolve(string reference, string? contextUrl)
        {
            if (entriesByUrl.ContainsKey(reference) || generated.ContainsKey(reference))
            {
                return reference;
            }

            if (!string.IsNullOrEmpty(contextUrl) && reference.Contains('/') && !reference.StartsWith("urn:", StringComparison.Ordinal))
            {
                var baseUrl = References.ExtractBaseUrl(contextUrl);
                var candidate = baseUrl.Length > 0 ? $"{baseUrl}/{reference}" : reference;
                if (entriesByUrl.ContainsKey(candidate))
                {
                    return candidate;
                }
            }

            return reference;
        }

        string RefOf(JsonNode? holder) => J.Str(holder, "reference") ?? string.Empty;

        var careTeam = J.BySequence(claim, "careTeam");
        var overall = careTeam.Values.FirstOrDefault(ct => J.IsTrue(J.ExtValue(ct, Sd("extension-careTeamClaimScope"), "valueBoolean")));
        var claimProvider = Resolve(RefOf(J.Get(claim, "provider")), claimUrl);

        JsonObject? RoleResource(string? roleUrl) =>
            roleUrl is not null && entriesByUrl.TryGetValue(roleUrl, out var entry) ? J.Obj(entry, "resource") : null;

        string? RolePractitioner(string? roleUrl)
        {
            var role = RoleResource(roleUrl);
            return J.Str(role, "resourceType") == "PractitionerRole" ? Resolve(RefOf(J.Get(role, "practitioner")), roleUrl) : null;
        }

        string? RoleLocation(string? roleUrl)
        {
            var first = J.Arr(RoleResource(roleUrl), "location").FirstOrDefault();
            return first is null ? null : Resolve(RefOf(first), roleUrl);
        }

        var supporting = J.BySequence(claim, "supportingInfo");
        var admission = supporting.Values.FirstOrDefault(s => J.FirstCode(J.Get(s, "category")) == "admissionDates");
        var discharge = supporting.Values.FirstOrDefault(s => J.FirstCode(J.Get(s, "category")) == "dischargeDates");
        var diagnoses = J.BySequence(claim, "diagnosis");

        JsonObject PreAuthPeriod() => new() { ["start"] = Today(), ["end"] = Today(rules.PreAuthPeriodDays) };

        JsonObject Adjudication(string decision, string? number = null, JsonNode? reason = null)
        {
            var d = rules.Decision(decision);
            var sub = new JsonArray();
            if (!string.IsNullOrEmpty(number))
            {
                sub.Add(new JsonObject { ["url"] = "number", ["valueString"] = number });
            }

            sub.Add(new JsonObject
            {
                ["url"] = Sd("extension-reviewActionCode"),
                ["valueCodeableConcept"] = new JsonObject
                {
                    ["coding"] = new JsonArray(new JsonObject
                    {
                        ["system"] = rules.ReviewActionCodeSystem,
                        ["code"] = J.Clone(J.Get(d, "code")),
                        ["display"] = J.Clone(J.Get(d, "display")),
                    }),
                },
            });
            if (J.Truthy(reason))
            {
                sub.Add(new JsonObject { ["url"] = "reasonCode", ["valueCodeableConcept"] = J.CodeableConcept(reason) });
            }

            return new JsonObject
            {
                ["extension"] = new JsonArray(new JsonObject { ["url"] = Sd("extension-reviewAction"), ["extension"] = sub }),
                ["category"] = new JsonObject { ["coding"] = new JsonArray(J.Coding(PasUrls.AdjudicationCategory, "submitted")) },
            };
        }

        JsonObject AdmissionDates() =>
            J.Truthy(J.Get(admission, "timingPeriod"))
                ? new JsonObject { ["url"] = Sd("extension-admissionDates"), ["valuePeriod"] = J.Clone(J.Get(admission, "timingPeriod")) }
                : WithOptional(new JsonObject { ["url"] = Sd("extension-admissionDates") }, "valueDate", J.Get(admission, "timingDate"));

        JsonObject DischargeDate() => new() { ["url"] = Sd("extension-dischargeDate"), ["valueDate"] = J.Clone(J.Get(discharge, "timingDate")) };

        var claimItems = J.Arr(claim, "item").Select(i => i as JsonObject ?? []).ToList();
        var items = new JsonArray();
        var addItems = new JsonArray();
        var errors = new JsonArray();
        (ItemResult Result, JsonObject Item)? communicationRequestNeeded = null;
        (ItemResult Result, JsonObject Item)? taskNeeded = null;

        foreach (var r in results)
        {
            var item = claimItems[r.Index];
            var seq = J.Get(item, "sequence");
            var seqText = J.PyStr(seq);
            var decision = r.Decision;
            var certified = rules.IsCertified(decision);
            var e = new JsonArray();

            var traceValue = J.ExtValue(item, Sd("extension-itemTraceNumber"), "valueIdentifier");
            JsonNode trace = J.Truthy(traceValue)
                ? traceValue!.DeepClone()
                : new JsonObject { ["system"] = PasUrls.PayerSid + "item-trace", ["value"] = $"NSHP-{identValue}-{seqText}" };
            e.Add(new JsonObject { ["url"] = Sd("extension-itemTraceNumber"), ["valueIdentifier"] = trace.DeepClone() });
            if (certified)
            {
                e.Add(new JsonObject { ["url"] = Sd("extension-itemPreAuthIssueDate"), ["valueDate"] = Today() });
                e.Add(new JsonObject { ["url"] = Sd("extension-itemPreAuthPeriod"), ["valuePeriod"] = PreAuthPeriod() });
            }

            var previous = J.Str(J.ExtValue(item, Sd("extension-authorizationNumber"), "valueString"));
            var currentNumber = certified ? rules.AuthorizationNumber(identValue, seqText) : null;
            if (!string.IsNullOrEmpty(previous))
            {
                if (operation == "inquire" && rules.ReplacedAuthorizations.TryGetValue(previous, out var replacement))
                {
                    e.Add(new JsonObject { ["url"] = Sd("extension-authorizationNumber"), ["valueString"] = replacement });
                    currentNumber = certified ? replacement : currentNumber;
                }
                else
                {
                    e.Add(new JsonObject { ["url"] = Sd("extension-authorizationNumber"), ["valueString"] = previous });
                }
            }

            var administrationReference = J.Str(J.ExtValue(item, Sd("extension-administrationReferenceNumber"), "valueString"));
            if (string.IsNullOrEmpty(administrationReference))
            {
                administrationReference = rules.AdministrationReferenceNumber(identValue, seqText);
            }

            e.Add(new JsonObject { ["url"] = Sd("extension-administrationReferenceNumber"), ["valueString"] = administrationReference });
            if (J.Truthy(J.Get(item, "servicedPeriod")))
            {
                e.Add(new JsonObject { ["url"] = Sd("extension-itemRequestedServiceDate"), ["valuePeriod"] = J.Clone(J.Get(item, "servicedPeriod")) });
            }
            else if (J.Truthy(J.Get(item, "servicedDate")))
            {
                e.Add(new JsonObject { ["url"] = Sd("extension-itemRequestedServiceDate"), ["valueDateTime"] = J.Clone(J.Get(item, "servicedDate")) });
            }

            string? providerRef = null;
            foreach (var careTeamSequence in J.Arr(item, "careTeamSequence"))
            {
                if (careTeam.TryGetValue(J.Key(careTeamSequence), out var member) &&
                    J.IsFalse(J.ExtValue(member, Sd("extension-careTeamClaimScope"), "valueBoolean")))
                {
                    providerRef = Resolve(RefOf(J.Get(member, "provider")), claimUrl);
                }
            }

            if (string.IsNullOrEmpty(providerRef) && overall is not null)
            {
                providerRef = Resolve(RefOf(J.Get(overall, "provider")), claimUrl);
            }

            if (string.IsNullOrEmpty(providerRef))
            {
                providerRef = claimProvider;
            }

            if (certified)
            {
                e.Add(new JsonObject
                {
                    ["url"] = Sd("extension-itemAuthorizedProvider"),
                    ["extension"] = new JsonArray(new JsonObject { ["url"] = "provider", ["valueReference"] = J.Reference(providerRef) }),
                });
            }

            var addSpec = decision == "modified" ? J.Obj(r.Rule, "addItem") : null;
            if (addSpec is not null)
            {
                e.Add(new JsonObject
                {
                    ["url"] = Sd("extension-itemAuthorizedDetail"),
                    ["extension"] = new JsonArray(
                        new JsonObject { ["url"] = "productOrServiceCode", ["valueCodeableConcept"] = J.CodeableConcept(J.Get(addSpec, "productOrService")) },
                        new JsonObject { ["url"] = "modifier", ["valueCodeableConcept"] = J.CodeableConcept(J.Get(addSpec, "modifier")) },
                        new JsonObject { ["url"] = "unitPrice", ["valueMoney"] = new JsonObject { ["value"] = J.Clone(J.Get(addSpec, "unitPriceUSD")), ["currency"] = "USD" } },
                        new JsonObject
                        {
                            // prof-2: a quantity with a value needs a unit or code (SPEC 4.5).
                            ["url"] = "quantity",
                            ["valueQuantity"] = _v221
                                ? new JsonObject { ["value"] = J.Clone(J.Get(addSpec, "quantity")), ["unit"] = "Units", ["system"] = PasUrls.X12 + "673", ["code"] = "FL" }
                                : new JsonObject { ["value"] = J.Clone(J.Get(addSpec, "quantity")), ["unit"] = "Units" },
                        }),
                });
            }

            if (_v221 && admission is not null)
            {
                e.Add(AdmissionDates());
            }

            if (_v221 && discharge is not null && J.Truthy(J.Get(discharge, "timingDate")))
            {
                e.Add(DischargeDate());
            }

            foreach (var diagnosisSequence in J.Arr(item, "diagnosisSequence"))
            {
                if (diagnoses.TryGetValue(J.Key(diagnosisSequence), out var diagnosis) && J.Truthy(J.Get(diagnosis, "diagnosisCodeableConcept")))
                {
                    e.Add(new JsonObject
                    {
                        ["url"] = Sd("extension-communicatedDiagnosis"),
                        ["valueCodeableConcept"] = J.Clone(J.Get(diagnosis, "diagnosisCodeableConcept")),
                    });
                    break;
                }
            }

            items.Add(new JsonObject
            {
                ["extension"] = e,
                ["itemSequence"] = J.Clone(seq),
                ["noteNumber"] = new JsonArray(JsonValue.Create(1)),
                ["adjudication"] = new JsonArray(Adjudication(decision, currentNumber, J.Get(r.Rule, "reviewActionReasonCode"))),
            });

            if (addSpec is not null)
            {
                var practitionerRef = RolePractitioner(providerRef);
                var locationRef = RoleLocation(providerRef);
                var addNumber = !string.IsNullOrEmpty(previous) ? previous : rules.AuthorizationNumber(identValue, seqText);
                var addItem = new JsonObject { ["itemSequence"] = new JsonArray(J.Clone(seq)) };
                if (_v221)
                {
                    var ae = new JsonArray
                    {
                        new JsonObject { ["url"] = Sd("extension-itemTraceNumber"), ["valueIdentifier"] = trace.DeepClone() },
                        new JsonObject { ["url"] = Sd("extension-itemPreAuthIssueDate"), ["valueDate"] = Today() },
                        new JsonObject { ["url"] = Sd("extension-itemPreAuthPeriod"), ["valuePeriod"] = PreAuthPeriod() },
                        new JsonObject { ["url"] = Sd("extension-authorizationNumber"), ["valueString"] = addNumber },
                        new JsonObject { ["url"] = Sd("extension-administrationReferenceNumber"), ["valueString"] = administrationReference },
                        new JsonObject { ["url"] = Sd("extension-epsdtIndicator"), ["valueBoolean"] = false },
                        new JsonObject { ["url"] = Sd("extension-nursingHomeResidentialStatus"), ["valueCodeableConcept"] = J.CodeableConcept(J.Coding(PasUrls.X12 + "1345", "2")) },
                        new JsonObject { ["url"] = Sd("extension-nursingHomeLevelOfCare"), ["valueCodeableConcept"] = J.CodeableConcept(J.Coding(PasUrls.X12 + "1337", "2")) },
                        new JsonObject { ["url"] = Sd("extension-productOrServiceCodeEnd"), ["valueCodeableConcept"] = J.CodeableConcept(J.Get(addSpec, "productOrService")) },
                        new JsonObject { ["url"] = Sd("extension-revenueCode"), ["valueCodeableConcept"] = J.CodeableConcept(J.Coding("https://www.nubc.org/CodeSystem/RevenueCodes", "0292")) },
                        new JsonObject { ["url"] = Sd("extension-revenueUnitRateLimit"), ["valueDecimal"] = J.Clone(J.Get(addSpec, "unitPriceUSD")) },
                    };
                    var sourceItem = r.StoredItem ?? item;
                    var requested = J.ExtValue(sourceItem, Sd("extension-requestedService"), "valueReference");
                    if (J.Truthy(requested))
                    {
                        var context = !string.IsNullOrEmpty(r.StoredUrl) ? r.StoredUrl : claimUrl;
                        ae.Add(new JsonObject { ["url"] = Sd("extension-requestedService"), ["valueReference"] = J.Reference(Resolve(RefOf(requested), context)) });
                    }

                    foreach (var name in new[] { "extension-serviceItemRequestType", "extension-certificationType" })
                    {
                        var value = J.ExtValue(item, Sd(name), "valueCodeableConcept");
                        if (J.Truthy(value))
                        {
                            ae.Add(new JsonObject { ["url"] = Sd(name), ["valueCodeableConcept"] = value!.DeepClone() });
                        }
                    }

                    if (admission is not null)
                    {
                        ae.Add(AdmissionDates());
                    }

                    if (discharge is not null && J.Truthy(J.Get(discharge, "timingDate")))
                    {
                        ae.Add(DischargeDate());
                    }

                    addItem["extension"] = ae;
                    if (!string.IsNullOrEmpty(practitionerRef))
                    {
                        addItem["provider"] = new JsonArray(new JsonObject
                        {
                            ["extension"] = new JsonArray(new JsonObject
                            {
                                ["url"] = Sd("extension-authorizedProviderType"),
                                ["valueCodeableConcept"] = J.CodeableConcept(J.Coding(PasUrls.X12 + "98", "SJ")),
                            }),
                            ["reference"] = practitionerRef,
                        });
                    }
                }

                addItem["productOrService"] = J.CodeableConcept(J.Get(addSpec, "productOrService"));
                addItem["modifier"] = new JsonArray(J.CodeableConcept(J.Get(addSpec, "modifier")));
                addItem["servicedPeriod"] = PreAuthPeriod();
                if (_v221 && !string.IsNullOrEmpty(locationRef))
                {
                    addItem["locationReference"] = J.Reference(locationRef);
                }

                addItem["quantity"] = _v221
                    ? new JsonObject { ["value"] = J.Clone(J.Get(addSpec, "quantity")), ["unit"] = "Units", ["system"] = PasUrls.X12 + "673", ["code"] = "FL" }
                    : new JsonObject { ["value"] = J.Clone(J.Get(addSpec, "quantity")) };
                addItem["unitPrice"] = new JsonObject { ["value"] = J.Clone(J.Get(addSpec, "unitPriceUSD")), ["currency"] = "USD" };
                addItem["adjudication"] = new JsonArray(Adjudication(J.Str(addSpec, "decision") ?? "approved", addNumber));
                addItems.Add(addItem);
            }

            if (J.Obj(r.Rule, "addError") is { Count: > 0 } errorSpec)
            {
                var ee = new JsonArray();
                if (_v221)
                {
                    ee.Add(new JsonObject
                    {
                        ["url"] = Sd("extension-errorElement"),
                        ["extension"] = new JsonArray(
                            new JsonObject { ["url"] = "processNote", ["valuePositiveInt"] = 1 },
                            new JsonObject { ["url"] = "error", ["valueString"] = J.Clone(J.Get(errorSpec, "errorElement")) }),
                    });
                }
                else
                {
                    ee.Add(new JsonObject { ["url"] = Sd("extension-errorElement"), ["valueString"] = J.Clone(J.Get(errorSpec, "errorElement")) });
                }

                var path = (J.Str(errorSpec, "errorPathTemplate") ?? "Claim.item[{itemIndex}]")
                    .Replace("{itemIndex}", r.Index.ToString(CultureInfo.InvariantCulture), StringComparison.Ordinal);
                ee.Add(new JsonObject { ["url"] = Sd("extension-errorPath"), ["valueString"] = path });
                ee.Add(new JsonObject { ["url"] = Sd("extension-errorFollowupAction"), ["valueCodeableConcept"] = J.CodeableConcept(J.Get(errorSpec, "followupAction")) });
                errors.Add(new JsonObject { ["extension"] = ee, ["itemSequence"] = J.Clone(seq), ["code"] = J.CodeableConcept(J.Get(errorSpec, "code")) });
            }

            if (decision == "pended" && J.Truthy(J.Get(r.Rule, "requestAdditionalInformation")))
            {
                if (operation == "submit")
                {
                    communicationRequestNeeded = (r, item);
                }

                taskNeeded = (r, item);
            }
        }

        var patientRef = Resolve(RefOf(J.Get(claim, "patient")), claimUrl);
        var insurerRef = Resolve(RefOf(J.Get(claim, "insurer")), claimUrl);
        var cr = new JsonObject
        {
            ["resourceType"] = "ClaimResponse",
            ["id"] = crId,
            ["meta"] = J.Profile(operation == "submit" ? "profile-claimresponse" : "profile-claiminquiryresponse"),
        };

        if (_v221)
        {
            var sender = new Dictionary<string, string?>(StringComparer.Ordinal);
            foreach (var part in (J.ExtValue(claim, Sd("extension-TransmissionIdentifiers"), "extension") as JsonArray ?? []).OfType<JsonObject>())
            {
                sender[J.Str(part, "url") ?? string.Empty] = J.Str(part, "valueString");
            }

            string? Sender(string key) => sender.TryGetValue(key, out var value) && !string.IsNullOrEmpty(value) ? value : null;

            var transmission = new JsonArray
            {
                new JsonObject { ["url"] = "applicationSenderCode", ["valueString"] = Sender("applicationReceiverCode") ?? rules.ApplicationSenderCode },
                new JsonObject { ["url"] = "applicationReceiverCode", ["valueString"] = Sender("applicationSenderCode") ?? "REQUESTOR" },
            };
            if (Sender("interchangeReceiverID") is { } interchangeSender)
            {
                transmission.Add(new JsonObject { ["url"] = "interchangeSenderID", ["valueString"] = interchangeSender });
            }

            if (Sender("interchangeSenderID") is { } interchangeReceiver)
            {
                transmission.Add(new JsonObject { ["url"] = "interchangeReceiverID", ["valueString"] = interchangeReceiver });
            }

            var humanReviewed = decisions.Any(d => d is "denied" or "modified") || results.Any(r => r.Finalized);
            cr["extension"] = new JsonArray(
                new JsonObject { ["url"] = Sd("extension-TransmissionIdentifiers"), ["extension"] = transmission },
                new JsonObject
                {
                    ["url"] = Sd("extension-claimResponseReviewer"),
                    ["extension"] = new JsonArray(
                        new JsonObject { ["url"] = "wasHumanReviewedFlag", ["valueBoolean"] = humanReviewed },
                        new JsonObject { ["url"] = "reviewerNPI", ["valueIdentifier"] = new JsonObject { ["system"] = PasUrls.Npi, ["value"] = rules.ReviewerNpi } }),
                },
                new JsonObject
                {
                    ["url"] = Sd("extension-itemAuthorizedProvider"),
                    ["extension"] = new JsonArray(new JsonObject
                    {
                        ["url"] = "provider",
                        ["valueReference"] = J.Reference(overall is not null ? Resolve(RefOf(J.Get(overall, "provider")), claimUrl) : claimProvider),
                    }),
                });
        }

        WithOptional(cr, "identifier", J.Get(claim, "identifier"));
        cr["status"] = "active";
        WithOptional(cr, "type", J.Get(claim, "type"));
        cr["use"] = "preauthorization";
        cr["patient"] = J.Reference(patientRef);
        cr["created"] = Instant;
        cr["insurer"] = J.Reference(insurerRef);
        cr["requestor"] = J.Reference(claimProvider);
        cr["request"] = claimUrl is not null && (claimUrl.StartsWith("http://", StringComparison.Ordinal) || claimUrl.StartsWith("https://", StringComparison.Ordinal))
            ? J.Reference(claimUrl)
            : new JsonObject { ["extension"] = new JsonArray(new JsonObject { ["url"] = PasUrls.DataAbsentReason, ["valueCode"] = "unknown" }) };
        cr["outcome"] = errors.Count > 0 ? "partial" : claimDecision == "pended" && !_v221 ? "queued" : "complete";
        cr["preAuthPeriod"] = PreAuthPeriod();

        var requestor = entriesByUrl.TryGetValue(claimProvider, out var requestorEntry) ? J.Obj(requestorEntry, "resource") : null;
        var requestorNpi = J.Objs(requestor, "identifier").Where(i => J.Str(i, "system") == PasUrls.Npi).Select(i => J.Str(i, "value")).FirstOrDefault() ?? "UNKNOWN";

        if (communicationRequestNeeded is var (commResult, commItem))
        {
            var spec = J.Obj(commResult.Rule, "requestAdditionalInformation")!;
            var crqId = FhirJson.NewId();
            var crqUrl = "urn:uuid:" + crqId;
            var itemSeq = J.Get(commItem, "sequence");
            var payloadExtensions = new JsonArray();
            if (!_v221)
            {
                payloadExtensions.Add(new JsonObject { ["url"] = Sd("extension-serviceLineNumber"), ["valuePositiveInt"] = J.Clone(itemSeq) });
            }

            var firstDiagnosisSequence = J.Arr(commItem, "diagnosisSequence").FirstOrDefault();
            if (firstDiagnosisSequence is not null && diagnoses.TryGetValue(J.Key(firstDiagnosisSequence), out var diagnosis))
            {
                payloadExtensions.Add(new JsonObject
                {
                    ["url"] = Sd("extension-communicatedDiagnosis"),
                    ["valueCodeableConcept"] = J.Clone(J.Get(diagnosis, "diagnosisCodeableConcept")),
                });
            }

            payloadExtensions.Add(new JsonObject { ["url"] = Sd("extension-contentModifier"), ["valueCodeableConcept"] = J.CodeableConcept(J.Get(spec, "contentModifier")) });

            var crq = new JsonObject
            {
                ["resourceType"] = "CommunicationRequest",
                ["id"] = crqId,
                ["meta"] = J.Profile("profile-communicationrequest"),
            };
            if (_v221)
            {
                crq["extension"] = new JsonArray(new JsonObject { ["url"] = Sd("extension-serviceLineNumber"), ["valuePositiveInt"] = J.Clone(itemSeq) });
            }

            crq["identifier"] = new JsonArray(new JsonObject
            {
                ["system"] = PasUrls.PayerSid + "communication-request-id",
                ["value"] = $"CR-{identValue}-{J.PyStr(itemSeq)}",
            });
            crq["status"] = "active";
            crq["category"] = new JsonArray(J.CodeableConcept(J.Get(spec, "communicationRequestCategory")));
            crq["medium"] = new JsonArray(J.CodeableConcept(J.Coding("http://terminology.hl7.org/CodeSystem/v3-ParticipationMode", "ELECTRONIC")));
            crq["subject"] = J.Reference(patientRef);
            crq["payload"] = new JsonArray(new JsonObject
            {
                ["extension"] = payloadExtensions,
                ["contentString"] = J.Clone(J.Get(J.Get(spec, "attachmentLoinc"), "code")),
            });
            crq["authoredOn"] = Instant;
            crq["requester"] = J.Reference(insurerRef);
            crq["recipient"] = new JsonArray(J.Reference(claimProvider));
            crq["sender"] = J.Reference(claimProvider);
            generated[crqUrl] = new JsonObject { ["fullUrl"] = crqUrl, ["resource"] = crq };
            cr["communicationRequest"] = new JsonArray(J.Reference(crqUrl));
        }

        cr["item"] = items;
        if (addItems.Count > 0)
        {
            cr["addItem"] = addItems;
        }

        // Header adjudication (SPEC 4.4): extension-reviewActionCode is only allowed on item and addItem adjudication,
        // so the claim level reviewAction carries the claim authorization number (certified only) and
        // secondSurgicalOpinionFlag; the claim level decision is expressed through the items and processNote.
        var headerReview = new JsonArray();
        if (rules.IsCertified(claimDecision))
        {
            headerReview.Add(new JsonObject { ["url"] = "number", ["valueString"] = rules.ClaimAuthorizationNumber(identValue) });
        }

        headerReview.Add(new JsonObject { ["url"] = "secondSurgicalOpinionFlag", ["valueBoolean"] = false });
        cr["adjudication"] = new JsonArray(new JsonObject
        {
            ["extension"] = new JsonArray(new JsonObject { ["url"] = Sd("extension-reviewAction"), ["extension"] = headerReview }),
            ["category"] = new JsonObject { ["coding"] = new JsonArray(J.Coding(PasUrls.AdjudicationCategory, "submitted")) },
        });
        var claimDecisionDefinition = rules.Decision(claimDecision);
        cr["processNote"] = new JsonArray(new JsonObject
        {
            ["number"] = 1,
            ["type"] = "display",
            ["text"] = $"Synthetic adjudication: claim decision {J.Str(claimDecisionDefinition, "code")} {J.Str(claimDecisionDefinition, "display")}.",
        });
        if (errors.Count > 0)
        {
            cr["error"] = errors;
        }

        JsonObject? task = null;
        string? taskId = null;
        if (taskNeeded is var (taskResult, taskItem))
        {
            var spec = J.Obj(taskResult.Rule, "requestAdditionalInformation")!;
            taskId = FhirJson.NewId();
            var itemSeq = J.Get(taskItem, "sequence");

            JsonObject reasonReference;
            if (claimUrl is not null && claimUrl.StartsWith("http", StringComparison.Ordinal))
            {
                reasonReference = J.Reference(claimUrl);
            }
            else
            {
                var identifier = new JsonObject();
                foreach (var (key, value) in J.Arr(claim, "identifier").FirstOrDefault() as JsonObject ?? [])
                {
                    if (key is "system" or "value")
                    {
                        identifier[key] = J.Clone(value);
                    }
                }

                reasonReference = new JsonObject { ["identifier"] = identifier };
            }

            var lineUrl = Sd(_v221 ? "extension-serviceLineNumber" : "extension-paLineNumber");
            JsonObject LineNumber() => _v221
                ? new JsonObject { ["url"] = lineUrl, ["valuePositiveInt"] = J.Clone(itemSeq) }
                : new JsonObject { ["url"] = lineUrl, ["valueInteger"] = J.Clone(itemSeq) };

            var attachmentExtensions = new JsonArray(LineNumber());
            if (_v221)
            {
                attachmentExtensions.Add(new JsonObject { ["url"] = Sd("extension-contentModifier"), ["valueCodeableConcept"] = J.CodeableConcept(J.Get(spec, "contentModifier")) });
            }

            var inputs = new JsonArray
            {
                new JsonObject { ["type"] = J.CodeableConcept(J.Coding(PasUrls.PasTempCodes, "payer-url")), ["valueUrl"] = _base },
                new JsonObject
                {
                    ["extension"] = attachmentExtensions,
                    ["type"] = J.CodeableConcept(J.Coding(PasUrls.PasTempCodes, "attachments-needed")),
                    ["valueCodeableConcept"] = J.CodeableConcept(new JsonObject { ["system"] = PasUrls.Loinc, ["code"] = J.Clone(J.Get(J.Get(spec, "attachmentLoinc"), "code")) }),
                },
            };
            if (_v221)
            {
                inputs.Add(new JsonObject
                {
                    ["extension"] = new JsonArray(LineNumber()),
                    ["type"] = J.CodeableConcept(J.Coding(PasUrls.PasTempCodes, "questionnaire-context")),
                    ["valueString"] = $"{_base}/Questionnaire/pas-home-oxygen-documentation",
                });
            }

            task = new JsonObject
            {
                ["resourceType"] = "Task",
                ["id"] = taskId,
                ["meta"] = J.Profile("profile-task"),
                ["identifier"] = new JsonArray(new JsonObject { ["system"] = PasUrls.PayerSid + "task-id", ["value"] = $"TASK-{identValue}-{J.PyStr(itemSeq)}" }),
                ["status"] = "requested",
                ["statusReason"] = new JsonObject { ["text"] = "Additional documentation is required to complete the prior authorization review" },
                ["intent"] = "order",
                ["code"] = J.CodeableConcept(J.Coding(PasUrls.PasTempCodes, "attachment-request-code")),
                ["for"] = J.Reference(patientRef),
                ["authoredOn"] = Instant,
                ["requester"] = new JsonObject { ["identifier"] = new JsonObject { ["system"] = PasUrls.PayerSid + "payer-id", ["value"] = "NSHP01" } },
                ["owner"] = new JsonObject { ["identifier"] = new JsonObject { ["system"] = PasUrls.Npi, ["value"] = requestorNpi } },
                ["reasonCode"] = J.CodeableConcept(J.Coding(PasUrls.PasTempCodes, "priorAuthorization")),
                ["reasonReference"] = reasonReference,
                ["restriction"] = new JsonObject { ["period"] = new JsonObject { ["start"] = Today(), ["end"] = Today(30) } },
                ["input"] = inputs,
            };
        }

        // Bundle closure: ClaimResponse first, then every resource reachable from it (request excluded), then
        // generated resources. References always equal entry fullUrls; Claims are never echoed.
        var outEntries = new JsonArray(new JsonObject { ["fullUrl"] = crUrl, ["resource"] = cr });
        var included = new HashSet<string>(StringComparer.Ordinal) { crUrl };

        void Include(string? url)
        {
            if (string.IsNullOrEmpty(url) || included.Contains(url))
            {
                return;
            }

            if (generated.TryGetValue(url, out var generatedEntry))
            {
                included.Add(url);
                outEntries.Add(generatedEntry);
                foreach (var reference in References.Iterate(J.Get(generatedEntry, "resource")).ToList())
                {
                    Include(Resolve(reference, url));
                }

                return;
            }

            if (!entriesByUrl.TryGetValue(url, out var entry) || J.Obj(entry, "resource") is not { } resource ||
                J.Str(resource, "resourceType") == "Claim")
            {
                return;
            }

            included.Add(url);
            outEntries.Add(new JsonObject { ["fullUrl"] = url, ["resource"] = resource.DeepClone() });
            foreach (var reference in References.Iterate(resource).ToList())
            {
                Include(Resolve(reference, url));
            }
        }

        foreach (var reference in References.Iterate(cr).ToList())
        {
            Include(Resolve(reference, claimUrl));
        }

        if (task is not null)
        {
            Include(patientRef);
            outEntries.Add(new JsonObject { ["fullUrl"] = "urn:uuid:" + taskId, ["resource"] = task });
        }

        return new JsonObject
        {
            ["resourceType"] = "Bundle",
            ["id"] = FhirJson.NewId(),
            ["meta"] = J.Profile(operation == "submit" ? "profile-pas-response-bundle" : "profile-pas-inquiry-response-bundle"),
            ["identifier"] = new JsonObject { ["system"] = "urn:ietf:rfc:3986", ["value"] = "urn:uuid:" + FhirJson.NewId() },
            ["type"] = "collection",
            ["timestamp"] = Instant,
            ["entry"] = outEntries,
        };
    }

    /// <summary>Claim.identifier[0].value, or UNKNOWN.</summary>
    public static string IdentifierValue(JsonObject claim) =>
        J.Arr(claim, "identifier").FirstOrDefault() is JsonObject first && first.TryGetPropertyValue("value", out var value)
            ? J.PyStr(value)
            : "UNKNOWN";

    private static JsonObject WithOptional(JsonObject target, string key, JsonNode? value)
    {
        if (value is not null)
        {
            target[key] = value.DeepClone();
        }

        return target;
    }
}
