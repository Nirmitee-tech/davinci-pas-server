using System.Globalization;
using Nirmitee.DaVinci.Pas;
using Nirmitee.DaVinci.Pas.Rules;

var builder = WebApplication.CreateBuilder(args);

// PAS_PORT (shared with the Go server) sets the listen port unless --urls or ASPNETCORE_URLS is given.
if (string.IsNullOrWhiteSpace(builder.Configuration["urls"]))
{
    var port = int.TryParse(builder.Configuration["PAS_PORT"], NumberStyles.Integer, CultureInfo.InvariantCulture, out var configured) && configured > 0
        ? configured
        : 8086;
    builder.WebHost.UseUrls($"http://0.0.0.0:{port}");
}

builder.Services.AddPas(builder.Configuration);

var app = builder.Build();

var prefix = PasEndpointRouteBuilderExtensions.NormalizePrefix(builder.Configuration["PAS_PREFIX"] ?? builder.Configuration["Pas:PathPrefix"]);
app.MapGet(prefix.Length == 0 ? "/" : prefix, () => Results.Json(new
{
    name = "Nirmitee Da Vinci PAS Server",
    fhirBases = new[] { $"{prefix}/v221/fhir", $"{prefix}/v201/fhir" },
    health = $"{prefix}/health",
}));
app.MapPas(prefix);

// Fail fast when the rules file is missing or invalid.
app.Services.GetRequiredService<AdjudicationRules>();

app.Run();

/// <summary>Entry point, public so integration tests can host the app.</summary>
public partial class Program;
