using System.Text.Json;
using System.Text.Json.Nodes;

namespace Nirmitee.DaVinci.Common.Json;

/// <summary>Helpers for reading FHIR and CDS Hooks JSON without a typed model.</summary>
public static class JsonUtil
{
    public static readonly JsonSerializerOptions Compact = new() { WriteIndented = false };

    public static string? GetString(JsonNode? node, string property) =>
        node is JsonObject obj && obj.TryGetPropertyValue(property, out var value) && value is JsonValue jv &&
        jv.TryGetValue<string>(out var s)
            ? s
            : null;

    public static bool? GetBool(JsonNode? node, string property) =>
        node is JsonObject obj && obj.TryGetPropertyValue(property, out var value) && value is JsonValue jv &&
        jv.TryGetValue<bool>(out var b)
            ? b
            : null;

    public static JsonObject? GetObject(JsonNode? node, string property) =>
        node is JsonObject obj && obj.TryGetPropertyValue(property, out var value) ? value as JsonObject : null;

    public static JsonArray? GetArray(JsonNode? node, string property) =>
        node is JsonObject obj && obj.TryGetPropertyValue(property, out var value) ? value as JsonArray : null;

    public static IEnumerable<string> GetStrings(JsonNode? node, string property) =>
        GetArray(node, property)?.Select(v => v is JsonValue jv && jv.TryGetValue<string>(out var s) ? s : null).OfType<string>() ?? [];

    public static string? ResourceType(JsonNode? resource) => GetString(resource, "resourceType");

    public static string? Id(JsonNode? resource) => GetString(resource, "id");

    public static string? Reference(JsonNode? resource) =>
        ResourceType(resource) is { } type && Id(resource) is { } id ? $"{type}/{id}" : null;

    /// <summary>Resource objects in a Bundle's entries.</summary>
    public static IEnumerable<JsonObject> BundleResources(JsonNode? bundle)
    {
        if (ResourceType(bundle) != "Bundle" || GetArray(bundle, "entry") is not { } entries)
        {
            yield break;
        }

        foreach (var entry in entries)
        {
            if (GetObject(entry, "resource") is { } resource)
            {
                yield return resource;
            }
        }
    }

    /// <summary>Splits a FHIR reference ("Type/id", an absolute URL, or one with _history) into type and id.</summary>
    public static (string? Type, string? Id) ParseReference(string? reference)
    {
        if (string.IsNullOrWhiteSpace(reference) || reference.StartsWith('#'))
        {
            return (null, null);
        }

        var path = reference.Split('?', '#')[0].TrimEnd('/');
        var parts = path.Split('/', StringSplitOptions.RemoveEmptyEntries).ToList();
        var historyIndex = parts.IndexOf("_history");
        if (historyIndex >= 0)
        {
            parts = parts.Take(historyIndex).ToList();
        }

        return parts.Count >= 2 ? (parts[^2], parts[^1]) : (null, null);
    }

    public static JsonObject CloneObject(JsonObject source) => (JsonObject)source.DeepClone();

    /// <summary>
    /// Forces a parsed node tree to build all of its child nodes. JsonNode instances created by JsonNode.Parse
    /// materialize lazily, which is not safe under concurrent first access; call this before sharing a tree
    /// across tasks.
    /// </summary>
    public static void Materialize(JsonNode? node)
    {
        switch (node)
        {
            case JsonObject obj:
                foreach (var (_, child) in obj)
                {
                    Materialize(child);
                }

                break;
            case JsonArray array:
                foreach (var child in array)
                {
                    Materialize(child);
                }

                break;
        }
    }

    /// <summary>Adds days to a yyyy-MM-dd date string.</summary>
    public static string AddDays(string isoDate, int days) =>
        DateOnly.ParseExact(isoDate, "yyyy-MM-dd").AddDays(days).ToString("yyyy-MM-dd");

    public static JsonArray ToArray(IEnumerable<JsonNode?> items)
    {
        var array = new JsonArray();
        foreach (var item in items)
        {
            array.Add(item);
        }

        return array;
    }

    public static JsonArray StringArray(params string[] values) => ToArray(values.Select(v => (JsonNode?)JsonValue.Create(v)));

    /// <summary>All codings of a CodeableConcept-like node.</summary>
    public static IEnumerable<JsonObject> Codings(JsonNode? codeableConcept) =>
        GetArray(codeableConcept, "coding")?.OfType<JsonObject>() ?? [];
}
