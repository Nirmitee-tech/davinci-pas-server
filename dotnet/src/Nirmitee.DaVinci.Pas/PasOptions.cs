using System.Globalization;
using Microsoft.Extensions.Configuration;
using Nirmitee.DaVinci.Common.Audit;
using Nirmitee.DaVinci.Common.Http;

namespace Nirmitee.DaVinci.Pas;

/// <summary>
/// PAS module configuration. Bound from the "Pas" configuration section, then overridden by the PAS_* environment
/// variables shared with the Go server (SPEC section 1). PAS_PORT and PAS_PREFIX are read by the host.
/// </summary>
public sealed class PasOptions
{
    public const string SectionName = "Pas";

    /// <summary>Fixed external origin used in notifications and CapabilityStatement URLs, for example https://x.ngrok-free.app.</summary>
    public string? PublicBaseUrl { get; set; }

    /// <summary>Adjudication rules file. Empty uses the copy of spec/adjudication-rules.json embedded in the library.</summary>
    public string? RulesFile { get; set; }

    /// <summary>Accepted bearer tokens. Empty (the default) disables authentication.</summary>
    public List<string> AuthTokens { get; set; } = [];

    /// <summary>Seconds after a $submit before auto-finalizable pended items are finalized; 0 disables the timer.</summary>
    public double PendedAutoFinalizeSeconds { get; set; } = 10;

    /// <summary>Retry interval for handshake and event notifications.</summary>
    public double NotificationRetrySeconds { get; set; } = 3;

    /// <summary>Stop retrying a notification after this many seconds.</summary>
    public double NotificationMaxSeconds { get; set; } = 280;

    /// <summary>Delay between the Subscription 201 response and the first handshake attempt.</summary>
    public double HandshakeDelaySeconds { get; set; } = 1;

    /// <summary>Timeout of one notification POST.</summary>
    public double NotificationTimeoutSeconds { get; set; } = 15;

    /// <summary>
    /// Enables the test automation routes (POST admin/pended/finalize, POST admin/reset, GET admin/notifications).
    /// Off by default; set PAS_ADMIN_ENABLED=true only for test runs.
    /// </summary>
    public bool EnableAdmin { get; set; }

    /// <summary>Data directory; the audit log is written to {DataDir}/pas-audit/{yyyy-MM-dd}.jsonl.</summary>
    public string DataDir { get; set; } = "./data";

    public bool AuditEnabled { get; set; } = true;

    /// <summary>Prefix rewrites applied to Subscription endpoints before delivery (for example localhost to host.docker.internal in a container).</summary>
    public List<UrlRewriteRule> NotificationUrlRewrites { get; set; } = [];

    /// <summary>Derived from <see cref="DataDir"/> and <see cref="AuditEnabled"/>.</summary>
    public AuditOptions Audit { get; } = new();

    /// <summary>Applies PAS_* environment overrides. Called once when the options are built.</summary>
    public void Finalize(IConfiguration configuration)
    {
        if (Value(configuration, "PAS_PUBLIC_BASE_URL") is { } publicBase)
        {
            PublicBaseUrl = publicBase;
        }

        if (Value(configuration, "PAS_RULES_FILE") is { } rulesFile)
        {
            RulesFile = rulesFile;
        }

        if (Value(configuration, "PAS_AUTH_TOKENS") is { } tokens)
        {
            AuthTokens = tokens.Split(',', StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries).ToList();
        }

        if (Seconds(configuration, "PAS_PENDED_AUTO_FINALIZE_SECONDS") is { } autoFinalize)
        {
            PendedAutoFinalizeSeconds = autoFinalize;
        }

        if (Seconds(configuration, "PAS_NOTIFICATION_RETRY_SECONDS") is { } retry)
        {
            NotificationRetrySeconds = retry;
        }

        if (Seconds(configuration, "PAS_NOTIFICATION_MAX_SECONDS") is { } max)
        {
            NotificationMaxSeconds = max;
        }

        // Test-only control routes: only PAS_ADMIN_ENABLED set to exactly "true" enables them (SPEC section 11).
        // A bound Pas:EnableAdmin value (appsettings.json, Pas__EnableAdmin) is ignored on purpose.
        EnableAdmin = configuration["PAS_ADMIN_ENABLED"] == "true";

        if (Value(configuration, "PAS_DATA_DIR") is { } dataDir)
        {
            DataDir = dataDir;
        }

        PublicBaseUrl = string.IsNullOrWhiteSpace(PublicBaseUrl) ? null : PublicBaseUrl.Trim().TrimEnd('/');
        AuthTokens = AuthTokens.Where(t => !string.IsNullOrWhiteSpace(t)).Select(t => t.Trim()).Distinct().ToList();
        NotificationRetrySeconds = Math.Max(0.05, NotificationRetrySeconds);
        NotificationMaxSeconds = Math.Max(0, NotificationMaxSeconds);
        HandshakeDelaySeconds = Math.Max(0, HandshakeDelaySeconds);
        NotificationTimeoutSeconds = NotificationTimeoutSeconds <= 0 ? 15 : NotificationTimeoutSeconds;
        Audit.Directory = Path.Combine(string.IsNullOrWhiteSpace(DataDir) ? "./data" : DataDir, "pas-audit");
        Audit.Enabled = AuditEnabled;
    }

    private static string? Value(IConfiguration configuration, string key) =>
        configuration[key] is { } value && !string.IsNullOrWhiteSpace(value) ? value.Trim() : null;

    private static double? Seconds(IConfiguration configuration, string key) =>
        double.TryParse(Value(configuration, key), NumberStyles.Float, CultureInfo.InvariantCulture, out var seconds) && seconds >= 0
            ? seconds
            : null;
}
