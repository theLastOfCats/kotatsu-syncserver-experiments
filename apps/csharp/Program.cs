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

var authLimit = ReadLimit("AUTH_RATE_LIMIT", 30);
var forgotLimit = ReadLimit("FORGOT_PASSWORD_RATE_LIMIT", 5);
var resetLimit = ReadLimit("RESET_PASSWORD_RATE_LIMIT", 10);
builder.Services.AddResponseCompression(options =>
{
    options.Providers.Add<GzipCompressionProvider>();
    options.EnableForHttps = true;
});
builder.Services.AddRateLimiter(options =>
{
    options.RejectionStatusCode = StatusCodes.Status429TooManyRequests;
    options.AddPolicy("auth", context => PerAddress(context, authLimit, TimeSpan.FromMinutes(1)));
    options.AddPolicy("forgot-password", context => PerAddress(context, forgotLimit, TimeSpan.FromMinutes(10)));
    options.AddPolicy("reset-password", context => PerAddress(context, resetLimit, TimeSpan.FromMinutes(10)));
});

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
