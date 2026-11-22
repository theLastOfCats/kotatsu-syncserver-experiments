package kotatsu

import io.ktor.server.cio.CIO
import io.ktor.server.engine.embeddedServer

internal fun startServer(config: Config, database: Database) {
    embeddedServer(CIO, host = config.bindHost, port = config.port) {
        applicationModule(database, config)
    }.start(wait = true)
}
