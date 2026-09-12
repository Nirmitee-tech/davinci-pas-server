using System.Globalization;
using System.Text.Json.Nodes;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;
using Nirmitee.DaVinci.Pas.Fhir;
using Nirmitee.DaVinci.Pas.Http;
using Nirmitee.DaVinci.Pas.Rules;
using Nirmitee.DaVinci.Pas.Storage;

namespace Nirmitee.DaVinci.Pas.Claims;

/// <summary>Result of a finalize pass: the ids of the rebuilt ClaimResponses and the number of queued notifications.</summary>
public sealed record FinalizeOutcome(IReadOnlyList<string> Finalized, int Notifications);

/// <summary>
/// PAS behavior: $submit, $inquire, pended finalization, Subscriptions and notification bodies. Every operation that
/// touches stored state runs under one lock, and stored JSON trees are private copies, so responses returned to
/// callers never share nodes with the stores.
/// </summary>
public sealed class PasEngine(
    AdjudicationRules rules,
    IClaimStore claims,
    ISubscriptionStore subscriptions,
    INotificationOutbox outbox,
    INotificationLog deliveries,
    IOptionsMonitor<PasOptions> options,
    TimeProvider time,
    ILogger<PasEngine> logger)
{
    public const string ServerVersion = "0.1.0";
    public const string Handshake = "handshake";
    public const string EventNotification = "event-notification";

    private readonly Lock _gate = new();

    public AdjudicationRules Rules => rules;

    // ------------------------------------------------------------------ CapabilityStatement (SPEC section 3)

    public PasResponse Metadata(string version, string fhirBase)
    {
        var label = PasVersions.Label(version);
        var baseUrl = fhirBase.TrimEnd('/');
        return PasResponse.Fhir(200, new JsonObject
        {
            ["resourceType"] = "CapabilityStatement",
            ["id"] = $"nirmitee-pas-server-{version}",
            ["url"] = $"{baseUrl}/metadata",
            ["version"] = ServerVersion,
            ["name"] = "NirmiteePASServer",
            ["title"] = $"Nirmitee PAS Server (Da Vinci PAS {label})",
            ["status"] = "active",
            ["experimental"] = true,
            ["date"] = "2026-09-12",
            ["publisher"] = "Nirmitee",
            ["description"] = $"Da Vinci Prior Authorization Support {label} payer server with synthetic adjudication rules.",
            ["kind"] = "instance",
            ["software"] = new JsonObject { ["name"] = "Nirmitee PAS Server", ["version"] = ServerVersion },
            ["implementation"] = new JsonObject { ["description"] = $"Nirmitee PAS Server {label}", ["url"] = baseUrl },
            ["fhirVersion"] = "4.0.1",
            ["format"] = new JsonArray(JsonValue.Create("application/fhir+json"), JsonValue.Create("json")),
            ["rest"] = new JsonArray(new JsonObject
            {
                ["mode"] = "server",
                ["resource"] = new JsonArray(
                    new JsonObject
                    {
                        ["type"] = "Claim",
                        ["operation"] = new JsonArray(
                            new JsonObject { ["name"] = "submit", ["definition"] = PasUrls.SubmitOperation },
                            new JsonObject { ["name"] = "inquire", ["definition"] = PasUrls.InquireOperation }),
                    },
                    new JsonObject { ["type"] = "ClaimResponse", ["interaction"] = new JsonArray(Interaction("read")) },
                    new JsonObject
                    {
                        ["type"] = "Subscription",
                        ["extension"] = new JsonArray(new JsonObject
                        {
                            ["url"] = PasUrls.Backport + "capabilitystatement-subscriptiontopic-canonical",
                            ["valueCanonical"] = PasUrls.Topic,
                        }),
                        ["interaction"] = new JsonArray(Interaction("create"), Interaction("read"), Interaction("update"), Interaction("delete")),
                    }),
            }),
        });

        static JsonObject Interaction(string code) => new() { ["code"] = code };
    }

    // ------------------------------------------------------------------ request validation (SPEC 4.1, 5.1)

    /// <summary>The 400 OperationOutcome for a request Bundle that cannot be processed, or null.</summary>
    public static JsonObject? Validate(JsonNode? body, string operation)
    {
        if (body is not JsonObject bundle || J.Str(bundle, "resourceType") != "Bundle")
        {
            return Outcomes.Create("structure", "The request body must be a PAS Request Bundle.", "Bundle");
        }

        var entries = J.Arr(bundle, "entry").ToList();
        if (J.Str(bundle, "type") != "collection")
        {
            return Outcomes.Create("invalid", "Bundle.type must be collection.", "Bundle.type");
        }

        if (entries.Count == 0)
        {
            return Outcomes.Create("required", "The PAS Bundle has no entries; the first entry must be a Claim.", "Bundle.entry");
        }

        var claim = operation == "inquire"
            ? entries.Select(e => J.Obj(e, "resource")).FirstOrDefault(r => J.Str(r, "resourceType") == "Claim")
            : J.Obj(entries[0], "resource");
        if (J.Str(claim, "resourceType") != "Claim")
        {
            return Outcomes.Create("invalid", "The first Bundle entry must be a Claim.", "Bundle.entry[0].resource");
        }

        var urls = entries.Select(e => J.Str(e, "fullUrl")).OfType<string>().ToList();
        foreach (var field in new[] { "patient", "insurer", "provider" })
        {
            var reference = J.Str(J.Get(claim, field), "reference");
            if (string.IsNullOrEmpty(reference) ||
                (!urls.Contains(reference, StringComparer.Ordinal) && !urls.Any(u => u.EndsWith("/" + reference, StringComparison.Ordinal))))
            {
                return Outcomes.Create("invalid", $"Claim.{field} must reference a resource included in the Bundle.", $"Claim.{field}");
            }
        }

        if (operation == "submit" && !J.Truthy(J.Get(claim, "item")))
        {
            return Outcomes.Create("required", "Claim.item is required for $submit.", "Claim.item");
        }

        return null;
    }

    public static string ClaimKey(JsonObject claim)
    {
        var identifier = J.Arr(claim, "identifier").FirstOrDefault();
        return $"{J.PyStr(J.Get(identifier, "system"))}|{J.PyStr(J.Get(identifier, "value"))}";
    }

    // ------------------------------------------------------------------ $submit (SPEC section 4)

    public PasResponse Submit(string version, string fhirBase, JsonNode? body)
    {
        if (Validate(body, "submit") is { } outcome)
        {
            return PasResponse.Fhir(400, outcome);
        }

        lock (_gate)
        {
            var now = time.GetUtcNow();
            var bundle = (JsonObject)body!.DeepClone();
            var firstEntry = J.Arr(bundle, "entry").First();
            var claim = J.Obj(firstEntry, "resource")!;
            var fullUrl = J.Str(firstEntry, "fullUrl");
            var results = Adjudicate(claim);

            var record = new ClaimRecord
            {
                Version = version,
                Key = ClaimKey(claim),
                Claim = claim,
                FullUrl = fullUrl,
                Bundle = bundle,
                Results = results,
                FhirBase = fhirBase,
            };

            var response = new ResponseBuilder(rules, version, fhirBase, now).Build("submit", claim, fullUrl, results, [bundle]);
            record.LastResponse = (JsonObject)response.DeepClone();
            var seconds = options.CurrentValue.PendedAutoFinalizeSeconds;
            if (seconds > 0 && results.Any(r => r.AwaitsAutoFinalize))
            {
                record.AutoFinalizeAt = now.AddSeconds(seconds);
            }

            claims.Save(record);
            StoreClaimResponse(version, record.LastResponse);
            return PasResponse.Fhir(200, response);
        }
    }

    // ------------------------------------------------------------------ $inquire (SPEC section 5)

    public PasResponse Inquire(string version, string fhirBase, JsonNode? body)
    {
        if (Validate(body, "inquire") is { } outcome)
        {
            return PasResponse.Fhir(400, outcome);
        }

        lock (_gate)
        {
            var now = time.GetUtcNow();
            var bundle = (JsonObject)body!.DeepClone();
            var claim = J.Objs(bundle, "entry").Select(e => J.Obj(e, "resource")).First(r => J.Str(r, "resourceType") == "Claim")!;
            var stored = claims.Find(version, ClaimKey(claim));
            var storedItems = stored is null ? [] : J.Arr(stored.Claim, "item").Select(i => i as JsonObject ?? []).ToList();

            var results = new List<ItemResult>();
            var index = 0;
            foreach (var item in J.Arr(claim, "item").Select(i => i as JsonObject ?? []))
            {
                var sequence = J.Get(item, "sequence");
                var found = stored?.Results.FirstOrDefault(r => JsonNode.DeepEquals(r.Sequence, sequence));
                if (found is not null)
                {
                    results.Add(new ItemResult
                    {
                        Index = index,
                        Sequence = J.Clone(found.Sequence),
                        Rule = found.Rule,
                        Decision = found.Decision,
                        Finalized = found.Finalized,
                        StoredItem = found.Index < storedItems.Count ? storedItems[found.Index] : null,
                        StoredUrl = stored!.FullUrl,
                    });
                }
                else
                {
                    var rule = rules.Match(claim, item);
                    results.Add(new ItemResult { Index = index, Sequence = J.Clone(sequence), Rule = rule, Decision = J.Str(rule, "decision")! });
                }

                index++;
            }

            List<JsonObject> sources = stored is null ? [bundle] : [bundle, stored.Bundle];
            var response = new ResponseBuilder(rules, version, fhirBase, now).Build("inquire", claim, stored?.FullUrl, results, sources);
            if (version == PasVersions.V221)
            {
                return PasResponse.Fhir(200, new JsonObject
                {
                    ["resourceType"] = "Parameters",
                    ["parameter"] = new JsonArray(new JsonObject { ["name"] = "return", ["resource"] = response }),
                });
            }

            return PasResponse.Fhir(200, response);
        }
    }

    public PasResponse ReadClaimResponse(string version, string id)
    {
        lock (_gate)
        {
            return claims.FindClaimResponse(version, id) is { } claimResponse
                ? PasResponse.Fhir(200, claimResponse.DeepClone())
                : PasResponse.Outcome(404, "not-found", $"ClaimResponse/{id} is not known.");
        }
    }

    // ------------------------------------------------------------------ finalize (SPEC 7.3)

    /// <summary>Finalizes every auto-finalizable pended item now (admin route), for one version or all.</summary>
    public FinalizeOutcome FinalizeAll(string? version) => Finalize(version, dueOnly: false);

    /// <summary>Finalizes the records whose auto finalize time has passed (timer).</summary>
    public FinalizeOutcome FinalizeDue() => Finalize(null, dueOnly: true);

    private FinalizeOutcome Finalize(string? version, bool dueOnly)
    {
        lock (_gate)
        {
            var now = time.GetUtcNow();
            var finalized = new List<string>();
            var notifications = 0;
            foreach (var record in claims.List(version))
            {
                if (dueOnly && (record.AutoFinalizeAt is null || record.AutoFinalizeAt > now))
                {
                    continue;
                }

                record.AutoFinalizeAt = null;
                var changed = false;
                foreach (var result in record.Results.Where(r => r.AwaitsAutoFinalize))
                {
                    result.Decision = J.Str(result.Rule, "finalDecision") ?? "approved";
                    result.Finalized = true;
                    changed = true;
                }

                if (!changed)
                {
                    claims.Save(record);
                    continue;
                }

                var response = new ResponseBuilder(rules, record.Version, record.FhirBase, now)
                    .Build("submit", record.Claim, record.FullUrl, record.Results, [record.Bundle]);
                record.LastResponse = response;
                claims.Save(record);
                StoreClaimResponse(record.Version, response);
                if (J.Str(ClaimResponseOf(response), "id") is { } id)
                {
                    finalized.Add(id);
                }

                notifications += QueueEventNotifications(record, response, now);
                logger.LogInformation("Finalized pended claim {Key} on {Version}; {Count} notification(s) queued", record.Key, record.Version, notifications);
            }

            return new FinalizeOutcome(finalized, notifications);
        }
    }

    // ------------------------------------------------------------------ Subscriptions (SPEC 7.1, 7.2)

    public PasResponse CreateSubscription(string version, string fhirBase, JsonNode? body)
    {
        if (body is not JsonObject input || J.Str(input, "resourceType") != "Subscription")
        {
            return PasResponse.Outcome(400, "structure", "The request body must be a Subscription resource.", "Subscription");
        }

        if (ValidateSubscription(input, out var content) is { Count: > 0 } errors)
        {
            return PasResponse.Outcome(422, "business-rule", string.Join("; ", errors), "Subscription");
        }

        lock (_gate)
        {
            var now = time.GetUtcNow();
            var resource = (JsonObject)input.DeepClone();
            var id = FhirJson.NewId();
            resource["id"] = id;
            resource["status"] = "requested";
            var record = new SubscriptionRecord { Version = version, Id = id, Resource = resource, Content = content, FhirBase = fhirBase.TrimEnd('/') };
            subscriptions.Save(record);
            QueueHandshake(record, now);

            var response = PasResponse.Fhir(201, resource.DeepClone());
            response.Headers["Location"] = $"{record.FhirBase}/Subscription/{id}";
            return response;
        }
    }

    public PasResponse ReadSubscription(string version, string id)
    {
        lock (_gate)
        {
            return subscriptions.Find(version, id) is { } record
                ? PasResponse.Fhir(200, record.Resource.DeepClone())
                : UnknownSubscription(id);
        }
    }

    public PasResponse UpdateSubscription(string version, string id, JsonNode? body)
    {
        if (body is not JsonObject input || J.Str(input, "resourceType") != "Subscription")
        {
            return PasResponse.Outcome(400, "structure", "The request body must be a Subscription resource.", "Subscription");
        }

        if (J.Str(input, "id") is { } bodyId && bodyId != id)
        {
            return PasResponse.Outcome(400, "invalid", $"Subscription.id '{bodyId}' does not match the URL id '{id}'.", "Subscription.id");
        }

        if (ValidateSubscription(input, out var content) is { Count: > 0 } errors)
        {
            return PasResponse.Outcome(422, "business-rule", string.Join("; ", errors), "Subscription");
        }

        lock (_gate)
        {
            if (subscriptions.Find(version, id) is not { } record)
            {
                return UnknownSubscription(id);
            }

            var resource = (JsonObject)input.DeepClone();
            resource["id"] = id;
            var endpointChanged = J.Str(J.Obj(resource, "channel"), "endpoint") != record.Endpoint;
            resource["status"] = endpointChanged ? "requested" : record.Status;
            record.Resource = resource;
            record.Content = content;
            subscriptions.Save(record);
            if (endpointChanged)
            {
                outbox.RemoveForSubscription(version, id);
                QueueHandshake(record, time.GetUtcNow());
            }

            return PasResponse.Fhir(200, resource.DeepClone());
        }
    }

    public PasResponse DeleteSubscription(string version, string id)
    {
        lock (_gate)
        {
            if (!subscriptions.Remove(version, id))
            {
                return UnknownSubscription(id);
            }

            outbox.RemoveForSubscription(version, id);
            return PasResponse.NoContent();
        }
    }

    /// <summary>GET Subscription/{id}/$status: a searchset Bundle with one SubscriptionStatus (type query-status).</summary>
    public PasResponse SubscriptionStatus(string version, string id)
    {
        lock (_gate)
        {
            if (subscriptions.Find(version, id) is not { } record)
            {
                return UnknownSubscription(id);
            }

            var now = time.GetUtcNow();
            var parameters = StatusParameters(record, "query-status", record.Status, null, now);
            return PasResponse.Fhir(200, new JsonObject
            {
                ["resourceType"] = "Bundle",
                ["id"] = FhirJson.NewId(),
                ["type"] = "searchset",
                ["timestamp"] = FhirJson.Instant(now),
                ["total"] = 1,
                ["entry"] = new JsonArray(new JsonObject
                {
                    ["fullUrl"] = "urn:uuid:" + J.Str(parameters, "id"),
                    ["resource"] = parameters,
                    ["search"] = new JsonObject { ["mode"] = "match" },
                }),
            });
        }
    }

    /// <summary>Called after a 2xx answer to a handshake: the Subscription becomes active.</summary>
    public void MarkHandshakeDelivered(string version, string id)
    {
        lock (_gate)
        {
            if (subscriptions.Find(version, id) is { Status: "requested" } record)
            {
                record.Resource["status"] = "active";
                subscriptions.Save(record);
            }
        }
    }

    /// <summary>Clears claims, subscriptions and queued notifications.</summary>
    public void Reset()
    {
        lock (_gate)
        {
            claims.Clear();
            subscriptions.Clear();
            outbox.Clear();
            deliveries.Clear();
        }
    }

    // ------------------------------------------------------------------ notifications (SPEC 7.2, 7.4)

    private void QueueHandshake(SubscriptionRecord record, DateTimeOffset now)
    {
        var parameters = StatusParameters(record, Handshake, "requested", null, now);
        var body = new JsonObject
        {
            ["resourceType"] = "Bundle",
            ["id"] = FhirJson.NewId(),
            ["type"] = "history",
            ["timestamp"] = FhirJson.Instant(now),
            ["entry"] = new JsonArray(StatusEntry(record, parameters)),
        };
        Enqueue(record, Handshake, body, now.AddSeconds(options.CurrentValue.HandshakeDelaySeconds), now);
    }

    private int QueueEventNotifications(ClaimRecord claimRecord, JsonObject response, DateTimeOffset now)
    {
        var count = 0;
        foreach (var record in subscriptions.List(claimRecord.Version))
        {
            if (record.Status != "active" || !FilterMatches(record, claimRecord))
            {
                continue;
            }

            record.Events++;
            subscriptions.Save(record);
            var entries = new JsonArray();
            if (record.Content == "full-resource")
            {
                var focus = "urn:uuid:" + J.Str(response, "id");
                entries.Add(StatusEntry(record, StatusParameters(record, EventNotification, "active", focus, now)));
                entries.Add(new JsonObject
                {
                    ["fullUrl"] = focus,
                    ["resource"] = NotificationCopy(response),
                    ["request"] = new JsonObject { ["method"] = "POST", ["url"] = $"{record.FhirBase}/Bundle" },
                    ["response"] = new JsonObject { ["status"] = "201" },
                });
            }
            else
            {
                var claimResponseId = J.Str(ClaimResponseOf(response), "id");
                var focus = $"{record.FhirBase}/ClaimResponse/{claimResponseId}";
                entries.Add(StatusEntry(record, StatusParameters(record, EventNotification, "active", focus, now)));
                entries.Add(new JsonObject
                {
                    ["fullUrl"] = focus,
                    ["request"] = new JsonObject { ["method"] = "PUT", ["url"] = $"ClaimResponse/{claimResponseId}" },
                    ["response"] = new JsonObject { ["status"] = "200" },
                });
            }

            var body = new JsonObject
            {
                ["resourceType"] = "Bundle",
                ["id"] = FhirJson.NewId(),
                ["type"] = "history",
                ["timestamp"] = FhirJson.Instant(now),
                ["entry"] = entries,
            };
            Enqueue(record, EventNotification, body, now, now);
            count++;
        }

        return count;
    }

    private void Enqueue(SubscriptionRecord record, string kind, JsonObject body, DateTimeOffset dueAt, DateTimeOffset now)
    {
        var notification = new OutboundNotification
        {
            Kind = kind,
            Version = record.Version,
            SubscriptionId = record.Id,
            Endpoint = record.Endpoint,
            Headers = ChannelHeaders(record.Resource),
            Body = body.ToJsonString(FhirJson.Options),
            DueAt = dueAt,
            GiveUpAt = now.AddSeconds(Math.Max(options.CurrentValue.NotificationMaxSeconds, dueAt > now ? (dueAt - now).TotalSeconds : 0)),
        };
        deliveries.Record(notification, now);
        outbox.Enqueue(notification);
    }

    private static JsonObject StatusParameters(SubscriptionRecord record, string kind, string status, string? focus, DateTimeOffset now)
    {
        var events = record.Events.ToString(CultureInfo.InvariantCulture);
        var parameters = new JsonArray
        {
            new JsonObject { ["name"] = "subscription", ["valueReference"] = J.Reference($"{record.FhirBase}/Subscription/{record.Id}") },
            new JsonObject { ["name"] = "topic", ["valueCanonical"] = PasUrls.Topic },
            new JsonObject { ["name"] = "status", ["valueCode"] = status },
            new JsonObject { ["name"] = "type", ["valueCode"] = kind },
            new JsonObject { ["name"] = "events-since-subscription-start", ["valueString"] = events },
        };
        if (focus is not null)
        {
            parameters.Add(new JsonObject
            {
                ["name"] = "notification-event",
                ["part"] = new JsonArray(
                    new JsonObject { ["name"] = "event-number", ["valueString"] = events },
                    new JsonObject { ["name"] = "timestamp", ["valueInstant"] = FhirJson.Instant(now) },
                    new JsonObject { ["name"] = "focus", ["valueReference"] = J.Reference(focus) }),
            });
        }

        return new JsonObject { ["resourceType"] = "Parameters", ["id"] = FhirJson.NewId(), ["parameter"] = parameters };
    }

    private static JsonObject StatusEntry(SubscriptionRecord record, JsonObject parameters) => new()
    {
        ["fullUrl"] = "urn:uuid:" + J.Str(parameters, "id"),
        ["resource"] = parameters,
        ["request"] = new JsonObject { ["method"] = "GET", ["url"] = $"{record.FhirBase}/Subscription/{record.Id}/$status" },
        ["response"] = new JsonObject { ["status"] = "200" },
    };

    /// <summary>Content-Type from channel.payload plus every channel.header entry ("Name: value").</summary>
    public static IReadOnlyList<KeyValuePair<string, string>> ChannelHeaders(JsonObject subscription)
    {
        var channel = J.Obj(subscription, "channel");
        var headers = new OrderedDictionary<string, string>(StringComparer.OrdinalIgnoreCase)
        {
            ["Content-Type"] = J.Str(channel, "payload") is { Length: > 0 } payload ? payload : "application/fhir+json",
        };
        foreach (var header in J.Arr(channel, "header").Select(J.Str).OfType<string>())
        {
            var separator = header.IndexOf(": ", StringComparison.Ordinal);
            var name = separator < 0 ? header : header[..separator];
            headers[name.Trim()] = separator < 0 ? string.Empty : header[(separator + 2)..];
        }

        return headers.ToList();
    }

    private static List<string> ValidateSubscription(JsonObject subscription, out string content)
    {
        var channel = J.Obj(subscription, "channel");
        content = J.Objs(J.Get(channel, "_payload"), "extension")
            .Where(x => J.Str(x, "url") == PasUrls.Backport + "backport-payload-content")
            .Select(x => J.Str(x, "valueCode"))
            .FirstOrDefault() ?? string.Empty;

        var errors = new List<string>();
        if (J.Str(subscription, "criteria") != PasUrls.Topic)
        {
            errors.Add("Subscription.criteria must be the PAS SubscriptionTopic canonical");
        }

        if (J.Str(channel, "type") != "rest-hook")
        {
            errors.Add("Subscription.channel.type must be rest-hook");
        }

        var endpoint = J.Str(channel, "endpoint");
        if (string.IsNullOrEmpty(endpoint))
        {
            errors.Add("Subscription.channel.endpoint is required");
        }
        else if (!Uri.TryCreate(endpoint, UriKind.Absolute, out var uri) || (uri.Scheme != Uri.UriSchemeHttp && uri.Scheme != Uri.UriSchemeHttps))
        {
            errors.Add("Subscription.channel.endpoint must be an absolute http or https URL");
        }

        if (content is not ("full-resource" or "id-only"))
        {
            errors.Add("backport-payload-content must be full-resource or id-only");
        }

        return errors;
    }

    /// <summary>org-identifier=[system|]value filters must match an identifier of the requestor Organization.</summary>
    private static bool FilterMatches(SubscriptionRecord subscription, ClaimRecord record)
    {
        var filters = J.Objs(J.Get(subscription.Resource, "_criteria"), "extension")
            .Where(x => J.Str(x, "url") == PasUrls.Backport + "backport-filter-criteria")
            .Select(x => J.Str(x, "valueString"))
            .OfType<string>()
            .ToList();
        if (filters.Count == 0)
        {
            return true;
        }

        var providerRef = J.Str(J.Get(record.Claim, "provider"), "reference") ?? string.Empty;
        var requestor = J.Objs(record.Bundle, "entry")
            .Where(e => J.Str(e, "fullUrl") is { } url && (url == providerRef || url.EndsWith("/" + providerRef, StringComparison.Ordinal)))
            .Select(e => J.Obj(e, "resource"))
            .FirstOrDefault();
        var identifiers = J.Objs(requestor, "identifier").Select(i => (System: J.Str(i, "system"), Value: J.Str(i, "value"))).ToList();

        foreach (var filter in filters)
        {
            var equals = filter.IndexOf('=');
            if (equals < 0 || filter[..equals].Trim() != "org-identifier")
            {
                continue;
            }

            var value = filter[(equals + 1)..].Trim();
            string? system = null;
            var bar = value.IndexOf('|');
            if (bar >= 0)
            {
                system = value[..bar];
                value = value[(bar + 1)..];
            }

            if (!identifiers.Any(i => i.Value == value && (string.IsNullOrEmpty(system) || i.System == system)))
            {
                return false;
            }
        }

        return true;
    }

    private static readonly Dictionary<string, HashSet<string>> NotificationOmit = new(StringComparer.Ordinal)
    {
        ["root"] = [PasUrls.Sd("extension-itemAuthorizedProvider")],
        ["item"] = [PasUrls.Sd("extension-communicatedDiagnosis")],
        ["addItem"] =
        [
            PasUrls.Sd("extension-productOrServiceCodeEnd"), PasUrls.Sd("extension-revenueUnitRateLimit"),
            PasUrls.Sd("extension-serviceItemRequestType"), PasUrls.Sd("extension-certificationType"),
        ],
    };

    /// <summary>
    /// Copy of a response Bundle for a full-resource notification (SPEC 7.4 notification copy rule): the nested
    /// ClaimResponse omits the extensions whose context validator errors the kit only suppresses at top level.
    /// </summary>
    internal static JsonObject NotificationCopy(JsonObject responseBundle)
    {
        var copy = (JsonObject)responseBundle.DeepClone();
        foreach (var claimResponse in J.Objs(copy, "entry").Select(e => J.Obj(e, "resource")).OfType<JsonObject>()
                     .Where(r => J.Str(r, "resourceType") == "ClaimResponse"))
        {
            if (J.Get(claimResponse, "extension") is JsonArray rootExtensions)
            {
                RemoveExtensions(rootExtensions, NotificationOmit["root"]);
                if (rootExtensions.Count == 0)
                {
                    claimResponse.Remove("extension");
                }
            }

            foreach (var key in new[] { "item", "addItem" })
            {
                foreach (var element in J.Objs(claimResponse, key))
                {
                    if (J.Get(element, "extension") is JsonArray extensions)
                    {
                        RemoveExtensions(extensions, NotificationOmit[key]);
                    }
                }
            }
        }

        return copy;

        static void RemoveExtensions(JsonArray extensions, HashSet<string> urls)
        {
            for (var i = extensions.Count - 1; i >= 0; i--)
            {
                if (J.Str(extensions[i], "url") is { } url && urls.Contains(url))
                {
                    extensions.RemoveAt(i);
                }
            }
        }
    }

    private void StoreClaimResponse(string version, JsonObject responseBundle)
    {
        if (ClaimResponseOf(responseBundle) is { } claimResponse)
        {
            claims.SaveClaimResponse(version, (JsonObject)claimResponse.DeepClone());
        }
    }

    private static JsonObject? ClaimResponseOf(JsonObject responseBundle) =>
        J.Objs(responseBundle, "entry").Select(e => J.Obj(e, "resource")).FirstOrDefault(r => J.Str(r, "resourceType") == "ClaimResponse");

    private List<ItemResult> Adjudicate(JsonObject claim) =>
        J.Arr(claim, "item").Select((node, index) =>
        {
            var item = node as JsonObject ?? [];
            var rule = rules.Match(claim, item);
            return new ItemResult { Index = index, Sequence = J.Clone(J.Get(item, "sequence")), Rule = rule, Decision = J.Str(rule, "decision")! };
        }).ToList();

    private static PasResponse UnknownSubscription(string id) => PasResponse.Outcome(404, "not-found", $"Subscription/{id} is not known.");
}
