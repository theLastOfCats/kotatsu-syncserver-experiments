package kotatsu

import io.ktor.http.ContentType
import io.ktor.http.HttpStatusCode
import io.ktor.serialization.kotlinx.json.json
import io.ktor.server.application.Application
import io.ktor.server.application.call
import io.ktor.server.application.install
import io.ktor.server.application.log
import io.ktor.server.plugins.BadRequestException
import io.ktor.server.plugins.NotFoundException
import io.ktor.server.plugins.autohead.AutoHeadResponse
import io.ktor.server.plugins.compression.Compression
import io.ktor.server.plugins.compression.deflate
import io.ktor.server.plugins.compression.gzip
import io.ktor.server.plugins.compression.minimumSize
import io.ktor.server.plugins.contentnegotiation.ContentNegotiation
import io.ktor.server.plugins.ratelimit.RateLimit
import io.ktor.server.plugins.ratelimit.RateLimitName
import io.ktor.server.plugins.statuspages.StatusPages
import io.ktor.server.response.respondText
import io.ktor.server.routing.get
import io.ktor.server.routing.routing
import kotlinx.serialization.ExperimentalSerializationApi
import kotlinx.serialization.json.Json
import kotlin.time.Duration.Companion.minutes

fun main() {
    val config = Config.fromEnvironment()
    Database(config).use { database ->
        initAuth(database)
        initSync(database)
        startServer(config, database)
    }
}

@OptIn(ExperimentalSerializationApi::class)
fun Application.applicationModule(database: Database, config: Config) {
    install(ContentNegotiation) {
        json(Json {
            ignoreUnknownKeys = true
            explicitNulls = true
            encodeDefaults = true
        })
    }
    install(Compression) {
        gzip()
        deflate()
        minimumSize(0)
    }
    install(StatusPages) {
        exception<BadRequestException> { call, _ ->
            call.respondText("Bad Request", status = HttpStatusCode.BadRequest)
        }
        exception<IllegalArgumentException> { call, cause ->
            call.respondText("400: $cause", status = HttpStatusCode.BadRequest)
        }
        exception<NotFoundException> { call, cause ->
            call.respondText("404: $cause", status = HttpStatusCode.NotFound)
        }
        exception<Throwable> { call, cause ->
            this@applicationModule.log.error("Unhandled request failure", cause)
            call.respondText("Internal Server Error", status = HttpStatusCode.InternalServerError)
        }
    }
    install(RateLimit) {
        register(RateLimitName("auth")) {
            requestKey { it.request.local.remoteHost }
            rateLimiter(limit = config.authRateLimit, refillPeriod = 1.minutes)
        }
        register(RateLimitName("forgotPassword")) {
            requestKey { it.request.local.remoteHost }
            rateLimiter(limit = 5, refillPeriod = 10.minutes)
        }
        register(RateLimitName("resetPassword")) {
            requestKey { it.request.local.remoteHost }
            rateLimiter(limit = 5, refillPeriod = 10.minutes)
        }
    }
    install(AutoHeadResponse)
    routing {
        get("/") { call.respondText("Alive", ContentType.Text.Plain) }
        authRoutes(database, config)
        syncRoutes(database, config)
    }
}
