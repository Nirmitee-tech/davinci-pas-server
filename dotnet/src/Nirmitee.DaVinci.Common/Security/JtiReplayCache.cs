using System.Collections.Concurrent;

namespace Nirmitee.DaVinci.Common.Security;

/// <summary>Remembers JWT ids until the given time so that a captured token cannot be replayed.</summary>
public sealed class JtiReplayCache(TimeProvider time)
{
    private static readonly TimeSpan PurgeInterval = TimeSpan.FromMinutes(1);

    private readonly ConcurrentDictionary<string, DateTimeOffset> _seen = new(StringComparer.Ordinal);
    private DateTimeOffset _lastPurge = DateTimeOffset.MinValue;

    /// <summary>Registers the jti until <paramref name="keepUntil"/>. Returns false when the issuer already used it.</summary>
    public bool TryRegister(string issuer, string jti, DateTimeOffset keepUntil)
    {
        var now = time.GetUtcNow();
        PurgeIfDue(now);
        return _seen.TryAdd($"{issuer}\n{jti}", keepUntil > now ? keepUntil : now.AddMinutes(1));
    }

    public int Count => _seen.Count;

    private void PurgeIfDue(DateTimeOffset now)
    {
        if (now - _lastPurge < PurgeInterval)
        {
            return;
        }

        _lastPurge = now;
        foreach (var (key, keepUntil) in _seen)
        {
            if (keepUntil < now)
            {
                _seen.TryRemove(key, out _);
            }
        }
    }
}
