using System.Globalization;
using System.Text.Encodings.Web;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;

namespace Nirmitee.DaVinci.Pas.Fhir;

/// <summary>Canonical URLs and code systems used by the PAS server.</summary>
public static class PasUrls
{
    public const string StructureDefinition = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/";
    public const string X12 = "https://codesystem.x12.org/005010/";
    public const string PasTempCodes = "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes";
    public const string Loinc = "http://loinc.org";
    public const string Npi = "http://hl7.org/fhir/sid/us-npi";
    public const string DataAbsentReason = "http://hl7.org/fhir/StructureDefinition/data-absent-reason";
    public const string Topic = "http://hl7.org/fhir/us/davinci-pas/SubscriptionTopic/PASSubscriptionTopic";
    public const string Backport = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/";
    public const string PayerSid = "https://pas.nirmitee.example/fhir/sid/";
    public const string AdjudicationCategory = "http://terminology.hl7.org/CodeSystem/adjudication";
    public const string SubmitOperation = "http://hl7.org/fhir/us/davinci-pas/OperationDefinition/Claim-submit";
    public const string InquireOperation = "http://hl7.org/fhir/us/davinci-pas/OperationDefinition/Claim-inquiry";

    public static string Sd(string name) => StructureDefinition + name;
}

/// <summary>The two FHIR bases served side by side.</summary>
public static class PasVersions
{
    public const string V221 = "v221";
    public const string V201 = "v201";

    public static readonly string[] All = [V221, V201];

    public static bool IsKnown(string? version) => version is V221 or V201;

    public static string Label(string version) => version == V221 ? "2.2.1" : "2.0.1";
}

/// <summary>OperationOutcome builder (SPEC section 4.1 shape: severity, code, details.text, diagnostics, expression).</summary>
public static class Outcomes
{
    public static JsonObject Create(string code, string message, string? expression = null, string severity = "error")
    {
        var issue = new JsonObject
        {
            ["severity"] = severity,
            ["code"] = code,
            ["details"] = new JsonObject { ["text"] = message },
            ["diagnostics"] = message,
        };
        if (expression is not null)
        {
            issue["expression"] = new JsonArray(JsonValue.Create(expression));
        }

        return new JsonObject { ["resourceType"] = "OperationOutcome", ["issue"] = new JsonArray(issue) };
    }
}

public static class FhirJson
{
    /// <summary>Compact output that keeps characters such as quotes and angle brackets readable.</summary>
    public static readonly JsonSerializerOptions Options = new()
    {
        Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping,
        WriteIndented = false,
    };

    /// <summary>FHIR instant in UTC with second precision, for example 2026-09-12T10:00:00Z.</summary>
    public static string Instant(DateTimeOffset time) =>
        time.UtcDateTime.ToString("yyyy-MM-dd'T'HH:mm:ss'Z'", CultureInfo.InvariantCulture);

    /// <summary>FHIR date (UTC) plus a number of days.</summary>
    public static string Date(DateTimeOffset time, int plusDays = 0) =>
        time.UtcDateTime.Date.AddDays(plusDays).ToString("yyyy-MM-dd", CultureInfo.InvariantCulture);

    public static string NewId() => Guid.NewGuid().ToString("D");

    /// <summary>Parses JSON and builds every child node so the tree is safe to read later; null when the text is not JSON.</summary>
    public static JsonNode? TryParse(string? text)
    {
        if (string.IsNullOrWhiteSpace(text))
        {
            return null;
        }

        try
        {
            var node = JsonNode.Parse(text);
            Common.Json.JsonUtil.Materialize(node);
            return node;
        }
        catch (Exception ex) when (ex is JsonException or ArgumentException or InvalidOperationException)
        {
            return null;
        }
    }
}

/// <summary>Small JSON navigation helpers with Python-like truthiness, used by the adjudication port.</summary>
internal static class J
{
    public static JsonNode? Get(JsonNode? node, string key) =>
        node is JsonObject obj && obj.TryGetPropertyValue(key, out var value) ? value : null;

    public static JsonObject? Obj(JsonNode? node, string key) => Get(node, key) as JsonObject;

    public static IEnumerable<JsonNode?> Arr(JsonNode? node, string key) =>
        Get(node, key) is JsonArray array ? array : Enumerable.Empty<JsonNode?>();

    public static IEnumerable<JsonObject> Objs(JsonNode? node, string key) => Arr(node, key).OfType<JsonObject>();

    public static string? Str(JsonNode? node) =>
        node is JsonValue value && value.GetValueKind() == JsonValueKind.String ? value.GetValue<string>() : null;

    public static string? Str(JsonNode? node, string key) => Str(Get(node, key));

    public static bool IsTrue(JsonNode? node) => node is JsonValue value && value.GetValueKind() == JsonValueKind.True;

    public static bool IsFalse(JsonNode? node) => node is JsonValue value && value.GetValueKind() == JsonValueKind.False;

    /// <summary>Python truthiness of a JSON value: null, "", [], {}, 0 and false are falsy.</summary>
    public static bool Truthy(JsonNode? node) => node switch
    {
        null => false,
        JsonObject obj => obj.Count > 0,
        JsonArray array => array.Count > 0,
        JsonValue value => value.GetValueKind() switch
        {
            JsonValueKind.String => value.GetValue<string>().Length > 0,
            JsonValueKind.False or JsonValueKind.Null or JsonValueKind.Undefined => false,
            JsonValueKind.Number => double.TryParse(value.ToJsonString(), NumberStyles.Float, CultureInfo.InvariantCulture, out var d) && d != 0,
            _ => true,
        },
        _ => true,
    };

    public static JsonNode? Clone(JsonNode? node) => node?.DeepClone();

    /// <summary>The <paramref name="key"/> property of the first extension with <paramref name="url"/>.</summary>
    public static JsonNode? ExtValue(JsonNode? resource, string url, string key)
    {
        foreach (var extension in Objs(resource, "extension"))
        {
            if (Str(extension, "url") == url)
            {
                return Get(extension, key);
            }
        }

        return null;
    }

    /// <summary>coding[0].code of a CodeableConcept.</summary>
    public static string? FirstCode(JsonNode? codeableConcept) => Str(Arr(codeableConcept, "coding").FirstOrDefault(), "code");

    /// <summary>Text of a value as Python's str() prints a parsed JSON scalar (None, True, 1, text).</summary>
    public static string PyStr(JsonNode? node) => node switch
    {
        null => "None",
        JsonValue value when value.GetValueKind() == JsonValueKind.String => value.GetValue<string>(),
        JsonValue value when value.GetValueKind() == JsonValueKind.True => "True",
        JsonValue value when value.GetValueKind() == JsonValueKind.False => "False",
        _ => node.ToJsonString(),
    };

    /// <summary>Dictionary key for a sequence-like value.</summary>
    public static string Key(JsonNode? node) => node?.ToJsonString() ?? "null";

    public static JsonObject CodeableConcept(JsonNode? coding) => new() { ["coding"] = new JsonArray(Clone(coding)) };

    public static JsonObject Coding(string system, string code) => new() { ["system"] = system, ["code"] = code };

    public static JsonObject Reference(string? reference) => new() { ["reference"] = reference };

    public static JsonObject Profile(string name) => new() { ["profile"] = new JsonArray(JsonValue.Create(PasUrls.Sd(name))) };

    /// <summary>Ordered map of the objects in <paramref name="key"/> by their sequence; later duplicates replace the value.</summary>
    public static OrderedDictionary<string, JsonObject> BySequence(JsonNode? node, string key)
    {
        var map = new OrderedDictionary<string, JsonObject>(StringComparer.Ordinal);
        foreach (var obj in Objs(node, key))
        {
            map[Key(Get(obj, "sequence"))] = obj;
        }

        return map;
    }
}

/// <summary>Reference helpers mirroring the test kit's pas_bundle_validation.rb.</summary>
internal static class References
{
    private static readonly Regex UrlParts = new(@"^([a-zA-Z][a-zA-Z0-9+.-]*)://([^/:?#]+)(?::\d+)?([^?#]*)", RegexOptions.CultureInvariant);

    /// <summary>Scheme and host (port dropped) plus the path without its last two segments, as the kit computes it.</summary>
    public static string ExtractBaseUrl(string? url)
    {
        if (string.IsNullOrWhiteSpace(url))
        {
            return string.Empty;
        }

        var match = UrlParts.Match(url);
        if (!match.Success)
        {
            return string.Empty;
        }

        var segments = match.Groups[3].Value.Split('/');
        var kept = segments.Length > 2 ? segments[..^2] : [];
        return $"{match.Groups[1].Value}://{match.Groups[2].Value}" + string.Join('/', kept);
    }

    /// <summary>Every Reference.reference string in document order, skipping ClaimResponse.request.</summary>
    public static IEnumerable<string> Iterate(JsonNode? value)
    {
        switch (value)
        {
            case JsonArray array:
                foreach (var child in array)
                {
                    foreach (var reference in Iterate(child))
                    {
                        yield return reference;
                    }
                }

                break;
            case JsonObject obj:
                if (J.Str(obj, "reference") is { } found)
                {
                    yield return found;
                    yield break;
                }

                var isClaimResponse = J.Str(obj, "resourceType") == "ClaimResponse";
                foreach (var (key, child) in obj)
                {
                    if (key == "request" && isClaimResponse)
                    {
                        continue;
                    }

                    if (child is JsonObject or JsonArray)
                    {
                        foreach (var reference in Iterate(child))
                        {
                            yield return reference;
                        }
                    }
                }

                break;
        }
    }
}
