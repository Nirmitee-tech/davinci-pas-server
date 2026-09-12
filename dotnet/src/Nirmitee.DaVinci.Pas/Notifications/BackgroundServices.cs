using System.Text;
using System.Text.Json.Nodes;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;
using Nirmitee.DaVinci.Common.Http;
using Nirmitee.DaVinci.Pas.Claims;
using Nirmitee.DaVinci.Pas.Http;
using Nirmitee.DaVinci.Pas.Storage;

namespace Nirmitee.DaVinci.Pas.Notifications;

/// <summary>
/// Delivers queued rest-hook notifications (SPEC 7.2 to 7.4): POST with the Subscription's headers, retry every
/// PAS_NOTIFICATION_RETRY_SECONDS while the answer is not 2xx, give up after PAS_NOTIFICATION_MAX_SECONDS, never resend
/// after a 2xx. A delivered handshake activates the Subscription.
/// </summary>
public sealed class NotificationDeliveryService(
    INotificationOutbox outbox,
    INotificationLog deliveries,
    PasEngine engine,
    IHttpClientFactory httpClients,
    IOptionsMonitor<PasOptions> options,
    PasAuditLog audit,
    TimeProvider time,
    ILogger<NotificationDeliveryService> logger) : BackgroundService
{
    public const string HttpClientName = "Nirmitee.DaVinci.Pas.Notifications";

    private static readonly TimeSpan PollInterval = TimeSpan.FromMilliseconds(100);

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        while (!stoppingToken.IsCancellationRequested)
        {
            try
            {
                foreach (var notification in outbox.TakeDue(time.GetUtcNow()))
                {
                    _ = DeliverAsync(notification, stoppingToken);
                }
            }
            catch (Exception ex)
            {
                logger.LogError(ex, "Reading the notification outbox failed");
            }

            try
            {
                await Task.Delay(PollInterval, stoppingToken);
            }
            catch (OperationCanceledException)
            {
                break;
            }
        }
    }

    internal async Task DeliverAsync(OutboundNotification notification, CancellationToken stoppingToken)
    {
        notification.Attempts++;
        var settings = options.CurrentValue;
        var url = UrlRewriter.Rewrite(notification.Endpoint, settings.NotificationUrlRewrites);
        int? status = null;
        string? error = null;

        try
        {
            using var request = new HttpRequestMessage(HttpMethod.Post, url);
            var content = new StringContent(notification.Body, Encoding.UTF8);
            content.Headers.ContentType = null;
            foreach (var (name, value) in notification.Headers)
            {
                if (name.Equals("Content-Type", StringComparison.OrdinalIgnoreCase))
                {
                    content.Headers.TryAddWithoutValidation("Content-Type", value);
                }
                else if (!request.Headers.TryAddWithoutValidation(name, value))
                {
                    content.Headers.TryAddWithoutValidation(name, value);
                }
            }

            request.Content = content;
            using var timeout = CancellationTokenSource.CreateLinkedTokenSource(stoppingToken);
            timeout.CancelAfter(TimeSpan.FromSeconds(settings.NotificationTimeoutSeconds));
            using var response = await httpClients.CreateClient(HttpClientName).SendAsync(request, timeout.Token);
            status = (int)response.StatusCode;
        }
        catch (OperationCanceledException) when (stoppingToken.IsCancellationRequested)
        {
            return;
        }
        catch (Exception ex)
        {
            error = ex.GetType().Name + ": " + ex.Message;
        }

        var now = time.GetUtcNow();
        var delivered = status is >= 200 and < 300;
        deliveries.Update(notification.Id, notification.Attempts, status, delivered);
        string outcome;
        try
        {
            if (delivered)
            {
                outbox.Complete(notification);
                if (notification.Kind == PasEngine.Handshake)
                {
                    engine.MarkHandshakeDelivered(notification.Version, notification.SubscriptionId);
                }

                outcome = "delivered";
            }
            else
            {
                var next = now.AddSeconds(settings.NotificationRetrySeconds);
                if (next > notification.GiveUpAt)
                {
                    outbox.Complete(notification);
                    outcome = "abandoned";
                    logger.LogWarning("Gave up on {Kind} for Subscription/{Id} after {Attempts} attempt(s); last status {Status} {Error}",
                        notification.Kind, notification.SubscriptionId, notification.Attempts, status, error);
                }
                else
                {
                    notification.DueAt = next;
                    outbox.Reschedule(notification);
                    outcome = "retry";
                }
            }
        }
        catch (Exception ex)
        {
            logger.LogError(ex, "Updating the notification outbox failed");
            outcome = "error";
        }

        await audit.AppendAsync(new JsonObject
        {
            ["timestamp"] = now.UtcDateTime.ToString("O"),
            ["module"] = "pas",
            ["version"] = notification.Version,
            ["operation"] = "notify-" + notification.Kind,
            ["method"] = "POST",
            ["path"] = url,
            ["status"] = status,
            ["subscriptionId"] = notification.SubscriptionId,
            ["attempt"] = notification.Attempts,
            ["outcome"] = outcome,
            ["detail"] = error is null ? null : AuditRedactor.Redact(error),
        });
    }
}

/// <summary>Finalizes pended items whose PAS_PENDED_AUTO_FINALIZE_SECONDS delay has passed.</summary>
public sealed class PendedFinalizerService(PasEngine engine, ILogger<PendedFinalizerService> logger) : BackgroundService
{
    private static readonly TimeSpan PollInterval = TimeSpan.FromMilliseconds(250);

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        while (!stoppingToken.IsCancellationRequested)
        {
            try
            {
                engine.FinalizeDue();
            }
            catch (Exception ex)
            {
                logger.LogError(ex, "Auto finalize failed");
            }

            try
            {
                await Task.Delay(PollInterval, stoppingToken);
            }
            catch (OperationCanceledException)
            {
                break;
            }
        }
    }
}
