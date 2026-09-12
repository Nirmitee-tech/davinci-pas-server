using System.Globalization;
using System.Text.Json.Nodes;
using Nirmitee.DaVinci.Pas.Fhir;

namespace Nirmitee.DaVinci.Pas.Rules;

/// <summary>
/// The deterministic adjudication rules (SPEC section 6), loaded unchanged from adjudication-rules.json. Rules are
/// evaluated per Claim.item in order and the first match wins.
/// </summary>
public sealed class AdjudicationRules
{
    public const string EmbeddedResourceName = "Nirmitee.DaVinci.Pas.adjudication-rules.json";

    private AdjudicationRules(JsonObject root, string source)
    {
        Root = root;
        Source = source;
        ItemRules = J.Objs(root, "itemRules").ToList();
        if (ItemRules.Count == 0)
        {
            throw new InvalidOperationException($"The adjudication rules in {source} have no itemRules.");
        }

        if (J.Obj(root, "decisions") is not { } decisions)
        {
            throw new InvalidOperationException($"The adjudication rules in {source} have no decisions.");
        }

        foreach (var rule in ItemRules)
        {
            var decision = J.Str(rule, "decision") ?? throw new InvalidOperationException($"Rule {J.Str(rule, "id")} in {source} has no decision.");
            if (!decisions.ContainsKey(decision))
            {
                throw new InvalidOperationException($"Rule {J.Str(rule, "id")} in {source} uses the unknown decision '{decision}'.");
            }
        }

        if (!ItemRules.Any(rule => J.Obj(rule, "when") is { Count: 0 }))
        {
            throw new InvalidOperationException($"The adjudication rules in {source} have no default rule (empty 'when').");
        }

        ReplacedAuthorizations = (J.Obj(root, "replacedAuthorizations") ?? [])
            .Where(p => J.Str(p.Value) is not null)
            .ToDictionary(p => p.Key, p => J.Str(p.Value)!, StringComparer.Ordinal);
    }

    public JsonObject Root { get; }

    /// <summary>Where the rules came from (a file path or the embedded resource).</summary>
    public string Source { get; }

    public IReadOnlyList<JsonObject> ItemRules { get; }

    public IReadOnlyDictionary<string, string> ReplacedAuthorizations { get; }

    public string RulesVersion => J.Str(Root, "rulesVersion") ?? "unknown";

    public string ReviewActionCodeSystem => J.Str(Root, "reviewActionCodeSystem") ?? PasUrls.X12 + "306";

    public int PreAuthPeriodDays =>
        int.TryParse(J.Get(Root, "preAuthPeriodDays")?.ToJsonString(), NumberStyles.Integer, CultureInfo.InvariantCulture, out var days) ? days : 90;

    public string ApplicationSenderCode => J.Str(J.Obj(Root, "payer"), "applicationSenderCode") ?? "PAYER";

    public string ReviewerNpi => J.Str(J.Obj(Root, "payer"), "reviewerNpi") ?? string.Empty;

    public JsonObject Decision(string name) =>
        J.Obj(J.Obj(Root, "decisions"), name) ?? throw new InvalidOperationException($"Unknown decision '{name}'.");

    public bool IsCertified(string decision) => J.IsTrue(J.Get(Decision(decision), "certified"));

    public string AuthorizationNumber(string claimIdentifierValue, string itemSequence) =>
        Format("authorizationNumberTemplate", "AUTH-{claimIdentifierValue}-{itemSequence}", claimIdentifierValue, itemSequence);

    /// <summary>Claim level authorization number for the header reviewAction (SPEC 4.4).</summary>
    public string ClaimAuthorizationNumber(string claimIdentifierValue) =>
        Format("claimAuthorizationNumberTemplate", "AUTH-{claimIdentifierValue}", claimIdentifierValue, string.Empty);

    public string AdministrationReferenceNumber(string claimIdentifierValue, string itemSequence) =>
        Format("administrationReferenceNumberTemplate", "ADMREF-{claimIdentifierValue}-{itemSequence}", claimIdentifierValue, itemSequence);

    /// <summary>The first rule that matches the item (SPEC section 6).</summary>
    public JsonObject Match(JsonObject claim, JsonObject item)
    {
        foreach (var rule in ItemRules)
        {
            var when = J.Obj(rule, "when") ?? [];
            if (J.Truthy(J.Get(when, "itemModifierExtensionInfoCancelled")))
            {
                if (J.Objs(item, "modifierExtension").Any(m =>
                        J.Str(m, "url") == PasUrls.Sd("modifierextension-infoCancelled") && J.IsTrue(J.Get(m, "valueBoolean"))))
                {
                    return rule;
                }

                continue;
            }

            if (when.ContainsKey("claimCertificationTypeCode"))
            {
                var code = J.FirstCode(J.ExtValue(claim, PasUrls.Sd("extension-certificationType"), "valueCodeableConcept"));
                if (code == J.Str(when, "claimCertificationTypeCode"))
                {
                    return rule;
                }

                continue;
            }

            if (when.ContainsKey("productOrServiceCodes"))
            {
                var wanted = J.Arr(when, "productOrServiceCodes").Select(J.Str).OfType<string>().ToHashSet(StringComparer.Ordinal);
                var codes = J.Objs(J.Get(item, "productOrService"), "coding").Select(c => J.Str(c, "code"));
                if (codes.Any(c => c is not null && wanted.Contains(c)))
                {
                    return rule;
                }

                continue;
            }

            if (when.Count == 0)
            {
                return rule;
            }
        }

        throw new InvalidOperationException("No adjudication rule matched and no default rule exists.");
    }

    /// <summary>Claim level decision from the item decisions (SPEC section 6, claimLevelDecision).</summary>
    public static string ClaimLevel(IReadOnlyCollection<string> decisions)
    {
        var live = decisions.Where(d => d != "cancelled").ToList();
        if (live.Count == 0)
        {
            return "cancelled";
        }

        if (live.Contains("pended"))
        {
            return "pended";
        }

        if (live.All(d => d == "denied"))
        {
            return "denied";
        }

        var certified = live.Any(d => d is "approved" or "modified" or "partial");
        if (live.Contains("denied") && certified)
        {
            return "partial";
        }

        return live.Contains("modified") ? "modified" : "approved";
    }

    /// <summary>Loads the rules from <paramref name="path"/> or, when empty, from the copy embedded in this assembly.</summary>
    public static AdjudicationRules Load(string? path)
    {
        if (string.IsNullOrWhiteSpace(path))
        {
            using var stream = typeof(AdjudicationRules).Assembly.GetManifestResourceStream(EmbeddedResourceName)
                ?? throw new InvalidOperationException($"The embedded resource {EmbeddedResourceName} is missing.");
            using var reader = new StreamReader(stream);
            return Parse(reader.ReadToEnd(), "embedded adjudication-rules.json");
        }

        var resolved = ResolvePath(path.Trim());
        return Parse(File.ReadAllText(resolved), resolved);
    }

    public static AdjudicationRules Parse(string json, string source) =>
        new(FhirJson.TryParse(json) as JsonObject ?? throw new InvalidOperationException($"{source} is not a JSON object."), source);

    private static string ResolvePath(string path)
    {
        if (Path.IsPathRooted(path))
        {
            return File.Exists(path) ? path : throw new FileNotFoundException($"Rules file not found: {path}", path);
        }

        for (var dir = new DirectoryInfo(Directory.GetCurrentDirectory()); dir is not null; dir = dir.Parent)
        {
            var candidate = Path.Combine(dir.FullName, path);
            if (File.Exists(candidate))
            {
                return candidate;
            }
        }

        throw new FileNotFoundException($"Rules file not found: {path} (searched upward from {Directory.GetCurrentDirectory()})", path);
    }

    private string Format(string key, string fallback, string claimIdentifierValue, string itemSequence) =>
        (J.Str(Root, key) ?? fallback)
        .Replace("{claimIdentifierValue}", claimIdentifierValue, StringComparison.Ordinal)
        .Replace("{itemSequence}", itemSequence, StringComparison.Ordinal);
}
