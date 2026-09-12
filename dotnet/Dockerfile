# Nirmitee Da Vinci PAS payer server (.NET)
#
# Build from the repository root so the shared adjudication rules can be embedded:
#   docker build -f dotnet/Dockerfile -t nirmitee-pas-dotnet .
# Run:
#   docker run --rm -p 8086:8086 nirmitee-pas-dotnet

FROM mcr.microsoft.com/dotnet/sdk:10.0 AS build
WORKDIR /repo

COPY dotnet/src/Nirmitee.DaVinci.Common/Nirmitee.DaVinci.Common.csproj dotnet/src/Nirmitee.DaVinci.Common/
COPY dotnet/src/Nirmitee.DaVinci.Pas/Nirmitee.DaVinci.Pas.csproj dotnet/src/Nirmitee.DaVinci.Pas/
COPY dotnet/src/Nirmitee.DaVinci.Pas.Host/Nirmitee.DaVinci.Pas.Host.csproj dotnet/src/Nirmitee.DaVinci.Pas.Host/
RUN dotnet restore dotnet/src/Nirmitee.DaVinci.Pas.Host/Nirmitee.DaVinci.Pas.Host.csproj

COPY spec/adjudication-rules.json spec/adjudication-rules.json
COPY dotnet/src/ dotnet/src/
RUN dotnet publish dotnet/src/Nirmitee.DaVinci.Pas.Host/Nirmitee.DaVinci.Pas.Host.csproj \
    -c Release -o /app --no-restore /p:UseAppHost=false

FROM mcr.microsoft.com/dotnet/aspnet:10.0 AS runtime
WORKDIR /app

COPY --from=build /app ./

# Test-only control routes stay disabled in the image (PAS_ADMIN_ENABLED is not set).
ENV PAS_PORT=8086 \
    PAS_DATA_DIR=/data \
    DOTNET_CLI_TELEMETRY_OPTOUT=1

# The aspnet image ships a non-root "app" user (APP_UID); the data directory must be writable by it.
RUN mkdir -p /data && chown -R "$APP_UID" /data
USER $APP_UID

EXPOSE 8086
ENTRYPOINT ["dotnet", "Nirmitee.DaVinci.Pas.Host.dll"]
