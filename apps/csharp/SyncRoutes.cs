using System;
using System.Globalization;
using System.Linq;
using System.Text.Json;
using System.Text.Json.Serialization.Metadata;
using System.Threading.Tasks;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;

public static class SyncRoutes
{
    public static void Initialize() => SyncStorage.Initialize();

    public static void Map(WebApplication app)
    {
        var api = app.MapGroup("").WithMetadata(new ApiRateLimitMarker());
        api.MapMethods("/resource/history", ["GET", "HEAD"], GetHistory);
        api.MapPost("/resource/history", PostHistory);
        api.MapMethods("/resource/favourites", ["GET", "HEAD"], GetFavourites);
        api.MapPost("/resource/favourites", PostFavourites);
        api.MapMethods("/manga", ["GET", "HEAD"], GetMangaList);
        api.MapMethods("/manga/{id}", ["GET", "HEAD"], GetManga);
    }

    private static long? AuthorizedUser(HttpContext context)
    {
        context.Response.Headers["Cache-Control"] = "no-store";
        long? userId = Auth.UserId(context);
        if (userId is null) context.Response.StatusCode = StatusCodes.Status401Unauthorized;
        return userId;
    }

    private static async Task WriteJson<T>(HttpContext context, T body, JsonTypeInfo<T> typeInfo)
    {
        context.Response.Headers["Cache-Control"] = "no-store";
        context.Response.ContentType = "application/json; charset=utf-8";
        if (!HttpMethods.IsHead(context.Request.Method))
            await JsonSerializer.SerializeAsync(context.Response.Body, body, typeInfo, context.RequestAborted);
    }

    private static async Task GetHistory(HttpContext context)
    {
        long? userId = AuthorizedUser(context);
        if (userId is not null) await WriteJson(context, SyncStorage.GetHistory(userId.Value), SyncModel.Json.SyncHistoryPackage);
    }

    private static async Task PostHistory(HttpContext context)
    {
        long? userId = AuthorizedUser(context);
        if (userId is null) return;
        try
        {
            var request = await JsonSerializer.DeserializeAsync(context.Request.Body, SyncModel.Json.SyncHistoryPackage, context.RequestAborted);
            SyncModel.Validate(request);
            var response = SyncStorage.PutHistory(userId.Value, request!);
            if (SyncModel.ContentEquals(response, request!)) context.Response.StatusCode = StatusCodes.Status204NoContent;
            else await WriteJson(context, response, SyncModel.Json.SyncHistoryPackage);
        }
        catch (JsonException)
        {
            context.Response.StatusCode = StatusCodes.Status400BadRequest;
        }
        catch (InvalidSyncPackageException)
        {
            context.Response.StatusCode = StatusCodes.Status400BadRequest;
        }
        catch (SyncAccountMissingException)
        {
            context.Response.StatusCode = StatusCodes.Status401Unauthorized;
        }
    }

    private static async Task GetFavourites(HttpContext context)
    {
        long? userId = AuthorizedUser(context);
        if (userId is not null) await WriteJson(context, SyncStorage.GetFavourites(userId.Value), SyncModel.Json.SyncFavouritesPackage);
    }

    private static async Task PostFavourites(HttpContext context)
    {
        long? userId = AuthorizedUser(context);
        if (userId is null) return;
        try
        {
            var request = await JsonSerializer.DeserializeAsync(context.Request.Body, SyncModel.Json.SyncFavouritesPackage, context.RequestAborted);
            SyncModel.Validate(request);
            var response = SyncStorage.PutFavourites(userId.Value, request!);
            if (SyncModel.ContentEquals(response, request!)) context.Response.StatusCode = StatusCodes.Status204NoContent;
            else await WriteJson(context, response, SyncModel.Json.SyncFavouritesPackage);
        }
        catch (JsonException)
        {
            context.Response.StatusCode = StatusCodes.Status400BadRequest;
        }
        catch (InvalidSyncPackageException)
        {
            context.Response.StatusCode = StatusCodes.Status400BadRequest;
        }
        catch (SyncAccountMissingException)
        {
            context.Response.StatusCode = StatusCodes.Status401Unauthorized;
        }
    }

    private static async Task BadQuery(HttpContext context, string parameter)
    {
        context.Response.StatusCode = StatusCodes.Status400BadRequest;
        context.Response.Headers["Cache-Control"] = "no-store";
        context.Response.ContentType = "text/plain; charset=utf-8";
        if (!HttpMethods.IsHead(context.Request.Method))
            await context.Response.WriteAsync($"400: java.lang.IllegalArgumentException: Parameter \"{parameter}\" is missing or invalid");
    }

    private static async Task MangaNotFound(HttpContext context)
    {
        context.Response.StatusCode = StatusCodes.Status404NotFound;
        context.Response.Headers["Cache-Control"] = "no-store";
        context.Response.ContentType = "text/plain; charset=utf-8";
        if (!HttpMethods.IsHead(context.Request.Method))
            await context.Response.WriteAsync("404: io.ktor.server.plugins.NotFoundException: Resource not found");
    }

    private static async Task GetMangaList(HttpContext context)
    {
        string? offsetText = context.Request.Query["offset"].FirstOrDefault();
        string? limitText = context.Request.Query["limit"].FirstOrDefault();
        if (!int.TryParse(offsetText, NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture, out int offset) || offset < 0)
        {
            await BadQuery(context, "offset");
            return;
        }
        if (!int.TryParse(limitText, NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture, out int limit) || limit < 0)
        {
            await BadQuery(context, "limit");
            return;
        }
        await WriteJson(context, SyncStorage.GetManga(offset, limit), SyncModel.Json.ListSyncManga);
    }

    private static async Task GetManga(HttpContext context)
    {
        string? idText = context.Request.RouteValues["id"]?.ToString();
        if (!long.TryParse(idText, NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture, out long id))
        {
            await MangaNotFound(context);
            return;
        }
        var manga = SyncStorage.GetManga(id);
        if (manga is null)
        {
            await MangaNotFound(context);
            return;
        }
        await WriteJson(context, manga, SyncModel.Json.SyncManga);
    }
}
