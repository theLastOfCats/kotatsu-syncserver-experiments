using System.Globalization;
using System.Text;
using System.Threading.RateLimiting;
using Microsoft.AspNetCore.ResponseCompression;

var builder = WebApplication.CreateBuilder(args);

// Do not log request URLs: password recovery links contain bearer tokens.
builder.Logging.AddFilter("Microsoft.AspNetCore.Hosting.Diagnostics", LogLevel.Warning);

var portText = Environment.GetEnvironmentVariable("PORT");
var configuredUrls = Environment.GetEnvironmentVariable("ASPNETCORE_URLS")
    ?? Environment.GetEnvironmentVariable("URLS");
if (!string.IsNullOrEmpty(portText) || string.IsNullOrEmpty(configuredUrls))
{
    portText = string.IsNullOrEmpty(portText) ? "8080" : portText;
    if (!int.TryParse(portText, NumberStyles.None, CultureInfo.InvariantCulture, out var port)
        || port is < 1 or > 65535)
        throw new InvalidOperationException("PORT must be a valid TCP port");
    var host = Environment.GetEnvironmentVariable("BIND_HOST");
    builder.WebHost.UseUrls($"http://{(string.IsNullOrWhiteSpace(host) ? "127.0.0.1" : host)}:{port}");
}

var apiLimit = ReadLimit("RATE_LIMIT_API_PER_SECOND", 200);
var authLimit = ReadLimit("RATE_LIMIT_AUTH_PER_MINUTE", 5);
var forgotLimit = ReadLimit("RATE_LIMIT_FORGOT_PER_10_MINUTES", 5);
var resetLimit = ReadLimit("RATE_LIMIT_RESET_PER_10_MINUTES", 5);
var passwordConcurrency = ReadLimit("RATE_LIMIT_PASSWORD_CONCURRENCY", 2);
builder.Services.AddResponseCompression(options =>
{
    options.Providers.Add<GzipCompressionProvider>();
    options.EnableForHttps = true;
});
builder.Services.AddRateLimiter(options =>
{
    options.RejectionStatusCode = StatusCodes.Status429TooManyRequests;
    options.GlobalLimiter = PartitionedRateLimiter.Create<HttpContext, string>(context =>
        context.GetEndpoint()?.Metadata.GetMetadata<ApiRateLimitMarker>() is null
            ? RateLimitPartition.GetNoLimiter("outside-api")
            : RateLimitPartition.GetFixedWindowLimiter("api", _ => new FixedWindowRateLimiterOptions
            {
                PermitLimit = apiLimit,
                Window = TimeSpan.FromSeconds(1),
                QueueLimit = 0,
                AutoReplenishment = true
            }));
    options.OnRejected = (rejection, _) =>
    {
        var seconds = rejection.Lease.TryGetMetadata(MetadataName.RetryAfter, out var retry)
            ? Math.Max(1, (int)Math.Ceiling(retry.TotalSeconds))
            : 1;
        rejection.HttpContext.Response.Headers.RetryAfter = seconds.ToString(CultureInfo.InvariantCulture);
        return ValueTask.CompletedTask;
    };
    options.AddPolicy("auth", context => PerAddress(context, authLimit, TimeSpan.FromMinutes(1)));
    options.AddPolicy("forgot-password", context => PerAddress(context, forgotLimit, TimeSpan.FromMinutes(10)));
    options.AddPolicy("reset-password", context => PerAddress(context, resetLimit, TimeSpan.FromMinutes(10)));
});

Auth.ConfigurePasswordConcurrency(passwordConcurrency);
Auth.Initialize();
SyncRoutes.Initialize();

var app = builder.Build();
// Android may append a path to a base URL that already ends in '/'. Route the
// resulting repeated slashes as a single slash without redirecting POST bodies.
app.Use((context, next) =>
{
    var path = context.Request.Path.Value;
    if (path is not null && path.Contains("//", StringComparison.Ordinal))
    {
        var normalized = new StringBuilder(path.Length);
        var previousSlash = false;
        foreach (var ch in path)
        {
            if (ch != '/' || !previousSlash) normalized.Append(ch);
            previousSlash = ch == '/';
        }
        context.Request.Path = normalized.ToString();
    }
    return next(context);
});
app.UseResponseCompression();
app.UseRouting();
app.UseRateLimiter();
Auth.Map(app);
SyncRoutes.Map(app);
app.Run();

static int ReadLimit(string name, int fallback)
{
    var value = Environment.GetEnvironmentVariable(name);
    if (string.IsNullOrEmpty(value)) return fallback;
    if (!int.TryParse(value, NumberStyles.None, CultureInfo.InvariantCulture, out var limit) || limit < 1)
        throw new InvalidOperationException($"{name} must be a positive integer");
    return limit;
}

static RateLimitPartition<string> PerAddress(HttpContext context, int limit, TimeSpan window) =>
    RateLimitPartition.GetFixedWindowLimiter(context.Connection.RemoteIpAddress?.ToString() ?? "unknown", _ =>
        new FixedWindowRateLimiterOptions
        {
            PermitLimit = limit,
            Window = window,
            QueueLimit = 0,
            AutoReplenishment = true
        });

public sealed class ApiRateLimitMarker { }
