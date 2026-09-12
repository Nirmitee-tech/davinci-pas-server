using System.Text.Json.Nodes;
using Nirmitee.DaVinci.Pas.Fhir;

namespace Nirmitee.DaVinci.Pas.Storage;

/// <summary>The adjudication result of one Claim.item.</summary>
public sealed class ItemResult
{
    /// <summary>Index of the item in the Claim the response is built from.</summary>
    public int Index { get; set; }

    public JsonNode? Sequence { get; init; }

    public required JsonObject Rule { get; init; }

    public required string Decision { get; set; }

    /// <summary>True once a pended decision was finalized.</summary>
    public bool Finalized { get; set; }

    /// <summary>Inquiry only: the matching item of the stored submit Claim.</summary>
    public JsonObject? StoredItem { get; init; }

    /// <summary>Inquiry only: the fullUrl of the stored submit Claim.</summary>
    public string? StoredUrl { get; init; }

    public bool AwaitsAutoFinalize => Decision == "pended" && J.Truthy(J.Get(Rule, "autoFinalize"));
}

/// <summary>A submitted prior authorization request, keyed by Claim.identifier[0] system|value within a FHIR base version.</summary>
public sealed class ClaimRecord
{
    public required string Version { get; init; }

    public required string Key { get; init; }

    /// <summary>The first entry of <see cref="Bundle"/>.</summary>
    public required JsonObject Claim { get; init; }

    public string? FullUrl { get; init; }

    /// <summary>A private copy of the request Bundle.</summary>
    public required JsonObject Bundle { get; init; }

    public required List<ItemResult> Results { get; init; }

    /// <summary>The FHIR base the request arrived on, used for responses built later (finalize).</summary>
    public required string FhirBase { get; init; }

    public JsonObject? LastResponse { get; set; }

    /// <summary>When auto finalize is due, or null.</summary>
    public DateTimeOffset? AutoFinalizeAt { get; set; }
}

/// <summary>A rest-hook Subscription created on a FHIR base.</summary>
public sealed class SubscriptionRecord
{
    public required string Version { get; init; }

    public required string Id { get; init; }

    public required JsonObject Resource { get; set; }

    /// <summary>full-resource or id-only.</summary>
    public required string Content { get; set; }

    public int Events { get; set; }

    /// <summary>The FHIR base used in notification references.</summary>
    public required string FhirBase { get; init; }

    public string Status => J.Str(Resource, "status") ?? "requested";

    public string Endpoint => J.Str(J.Obj(Resource, "channel"), "endpoint") ?? string.Empty;
}

/// <summary>A notification waiting for delivery (handshake or event-notification).</summary>
public sealed class OutboundNotification
{
    public Guid Id { get; } = Guid.NewGuid();

    public required string Kind { get; init; }

    public required string Version { get; init; }

    public required string SubscriptionId { get; init; }

    public required string Endpoint { get; init; }

    public required IReadOnlyList<KeyValuePair<string, string>> Headers { get; init; }

    public required string Body { get; init; }

    public DateTimeOffset DueAt { get; set; }

    /// <summary>No attempt is made after this time.</summary>
    public DateTimeOffset GiveUpAt { get; init; }

    public int Attempts { get; set; }
}

/// <summary>Claim records and built ClaimResponses. Implementations must be thread safe.</summary>
public interface IClaimStore
{
    ClaimRecord? Find(string version, string key);

    /// <summary>Inserts or replaces the record for (version, key); a replaced record keeps its position.</summary>
    void Save(ClaimRecord record);

    /// <summary>Records in insertion order, for one version or all when null.</summary>
    IReadOnlyList<ClaimRecord> List(string? version);

    void SaveClaimResponse(string version, JsonObject claimResponse);

    JsonObject? FindClaimResponse(string version, string id);

    void Clear();
}

/// <summary>Subscriptions. Implementations must be thread safe.</summary>
public interface ISubscriptionStore
{
    void Save(SubscriptionRecord record);

    SubscriptionRecord? Find(string version, string id);

    IReadOnlyList<SubscriptionRecord> List(string version);

    bool Remove(string version, string id);

    void Clear();
}

/// <summary>Notifications waiting for delivery. Implementations must be thread safe.</summary>
public interface INotificationOutbox
{
    void Enqueue(OutboundNotification notification);

    /// <summary>Removes and returns the notifications due at <paramref name="now"/>; they stay in flight until completed or rescheduled.</summary>
    IReadOnlyList<OutboundNotification> TakeDue(DateTimeOffset now);

    /// <summary>Puts an in-flight notification back with its new <see cref="OutboundNotification.DueAt"/>.</summary>
    void Reschedule(OutboundNotification notification);

    /// <summary>Forgets an in-flight notification (delivered or abandoned).</summary>
    void Complete(OutboundNotification notification);

    void RemoveForSubscription(string version, string subscriptionId);

    /// <summary>Queued and in-flight notifications.</summary>
    int Count { get; }

    void Clear();
}

/// <summary>One line of the notification delivery log (SPEC section 11, GET admin/notifications).</summary>
public sealed class NotificationDelivery
{
    public required Guid Id { get; init; }

    public required string Version { get; init; }

    public required string Kind { get; init; }

    public required string SubscriptionId { get; init; }

    /// <summary>The bearer token sent to the endpoint (the subscriber's notification token), when present.</summary>
    public string? EndpointToken { get; init; }

    public int Attempts { get; set; }

    public int? Status { get; set; }

    public bool Delivered { get; set; }

    public required DateTimeOffset QueuedAt { get; init; }
}

/// <summary>Delivery log of handshake and event notifications. Implementations must be thread safe.</summary>
public interface INotificationLog
{
    void Record(OutboundNotification notification, DateTimeOffset queuedAt);

    void Update(Guid notificationId, int attempts, int? status, bool delivered);

    /// <summary>Snapshot copies, oldest first.</summary>
    IReadOnlyList<NotificationDelivery> List();

    void Clear();
}

public sealed class InMemoryNotificationLog : INotificationLog
{
    private const int Capacity = 1000;
    private readonly Lock _gate = new();
    private readonly OrderedDictionary<Guid, NotificationDelivery> _entries = new();

    public void Record(OutboundNotification notification, DateTimeOffset queuedAt)
    {
        var authorization = notification.Headers.FirstOrDefault(h => h.Key.Equals("Authorization", StringComparison.OrdinalIgnoreCase)).Value;
        lock (_gate)
        {
            if (_entries.Count >= Capacity)
            {
                _entries.RemoveAt(0);
            }

            _entries[notification.Id] = new NotificationDelivery
            {
                Id = notification.Id,
                Version = notification.Version,
                Kind = notification.Kind,
                SubscriptionId = notification.SubscriptionId,
                EndpointToken = authorization is not null && authorization.StartsWith("Bearer ", StringComparison.OrdinalIgnoreCase)
                    ? authorization["Bearer ".Length..].Trim()
                    : null,
                QueuedAt = queuedAt,
            };
        }
    }

    public void Update(Guid notificationId, int attempts, int? status, bool delivered)
    {
        lock (_gate)
        {
            if (_entries.TryGetValue(notificationId, out var entry))
            {
                entry.Attempts = attempts;
                entry.Status = status;
                entry.Delivered = delivered;
            }
        }
    }

    public IReadOnlyList<NotificationDelivery> List()
    {
        lock (_gate)
        {
            return _entries.Values.Select(e => new NotificationDelivery
            {
                Id = e.Id,
                Version = e.Version,
                Kind = e.Kind,
                SubscriptionId = e.SubscriptionId,
                EndpointToken = e.EndpointToken,
                Attempts = e.Attempts,
                Status = e.Status,
                Delivered = e.Delivered,
                QueuedAt = e.QueuedAt,
            }).ToList();
        }
    }

    public void Clear()
    {
        lock (_gate)
        {
            _entries.Clear();
        }
    }
}

public sealed class InMemoryClaimStore : IClaimStore
{
    private readonly Lock _gate = new();
    private readonly OrderedDictionary<(string Version, string Key), ClaimRecord> _records = new();
    private readonly Dictionary<(string Version, string Id), JsonObject> _claimResponses = new();

    public ClaimRecord? Find(string version, string key)
    {
        lock (_gate)
        {
            return _records.GetValueOrDefault((version, key));
        }
    }

    public void Save(ClaimRecord record)
    {
        lock (_gate)
        {
            _records[(record.Version, record.Key)] = record;
        }
    }

    public IReadOnlyList<ClaimRecord> List(string? version)
    {
        lock (_gate)
        {
            return _records.Values.Where(r => version is null || r.Version == version).ToList();
        }
    }

    public void SaveClaimResponse(string version, JsonObject claimResponse)
    {
        if (J.Str(claimResponse, "id") is not { } id)
        {
            return;
        }

        lock (_gate)
        {
            _claimResponses[(version, id)] = claimResponse;
        }
    }

    public JsonObject? FindClaimResponse(string version, string id)
    {
        lock (_gate)
        {
            return _claimResponses.GetValueOrDefault((version, id));
        }
    }

    public void Clear()
    {
        lock (_gate)
        {
            _records.Clear();
            _claimResponses.Clear();
        }
    }
}

public sealed class InMemorySubscriptionStore : ISubscriptionStore
{
    private readonly Lock _gate = new();
    private readonly OrderedDictionary<(string Version, string Id), SubscriptionRecord> _subscriptions = new();

    public void Save(SubscriptionRecord record)
    {
        lock (_gate)
        {
            _subscriptions[(record.Version, record.Id)] = record;
        }
    }

    public SubscriptionRecord? Find(string version, string id)
    {
        lock (_gate)
        {
            return _subscriptions.GetValueOrDefault((version, id));
        }
    }

    public IReadOnlyList<SubscriptionRecord> List(string version)
    {
        lock (_gate)
        {
            return _subscriptions.Values.Where(s => s.Version == version).ToList();
        }
    }

    public bool Remove(string version, string id)
    {
        lock (_gate)
        {
            return _subscriptions.Remove((version, id));
        }
    }

    public void Clear()
    {
        lock (_gate)
        {
            _subscriptions.Clear();
        }
    }
}

public sealed class InMemoryNotificationOutbox : INotificationOutbox
{
    private readonly Lock _gate = new();
    private readonly List<OutboundNotification> _queued = [];
    private readonly Dictionary<Guid, OutboundNotification> _inFlight = [];

    public int Count
    {
        get
        {
            lock (_gate)
            {
                return _queued.Count + _inFlight.Count;
            }
        }
    }

    public void Enqueue(OutboundNotification notification)
    {
        lock (_gate)
        {
            _queued.Add(notification);
        }
    }

    public IReadOnlyList<OutboundNotification> TakeDue(DateTimeOffset now)
    {
        lock (_gate)
        {
            var due = _queued.Where(n => n.DueAt <= now).ToList();
            foreach (var notification in due)
            {
                _queued.Remove(notification);
                _inFlight[notification.Id] = notification;
            }

            return due;
        }
    }

    public void Reschedule(OutboundNotification notification)
    {
        lock (_gate)
        {
            // Dropped by Clear or RemoveForSubscription while in flight: do not resurrect it.
            if (_inFlight.Remove(notification.Id))
            {
                _queued.Add(notification);
            }
        }
    }

    public void Complete(OutboundNotification notification)
    {
        lock (_gate)
        {
            _inFlight.Remove(notification.Id);
        }
    }

    public void RemoveForSubscription(string version, string subscriptionId)
    {
        lock (_gate)
        {
            _queued.RemoveAll(n => n.Version == version && n.SubscriptionId == subscriptionId);
            foreach (var id in _inFlight.Values.Where(n => n.Version == version && n.SubscriptionId == subscriptionId).Select(n => n.Id).ToList())
            {
                _inFlight.Remove(id);
            }
        }
    }

    public void Clear()
    {
        lock (_gate)
        {
            _queued.Clear();
            _inFlight.Clear();
        }
    }
}
