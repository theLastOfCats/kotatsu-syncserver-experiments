package org.kotatsu.bench.reference;

import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Collection;
import org.flywaydb.core.api.FlywayException;
import org.flywaydb.core.api.Location;
import org.flywaydb.core.api.configuration.Configuration;
import org.flywaydb.core.api.resource.LoadableResource;
import org.flywaydb.core.internal.resource.classpath.ClassPathResource;
import org.flywaydb.core.internal.scanner.classpath.ClasspathLocationHandlerImpl;

/** Lets Flyway discover the original JAR's migrations in Native Image's resource: URLs. */
public final class ReferenceNativeMigrations extends ClasspathLocationHandlerImpl {
    private static final String LOCATION = "db/migration";
    private static final String MANIFEST = "META-INF/reference-native/migrations.list";

    @Override
    public int getPriority() {
        return 1;
    }

    @Override
    public Collection<LoadableResource> scanForResources(Location location, Configuration configuration) {
        if (!LOCATION.equals(location.getPath())) {
            return super.scanForResources(location, configuration);
        }

        ClassLoader loader = configuration.getClassLoader();
        InputStream manifest = loader.getResourceAsStream(MANIFEST);
        if (manifest == null) {
            throw new FlywayException("Missing embedded reference migration manifest");
        }
        Collection<LoadableResource> resources = new ArrayList<>();
        try (BufferedReader reader = new BufferedReader(new InputStreamReader(manifest, StandardCharsets.UTF_8))) {
            for (String name; (name = reader.readLine()) != null; ) {
                if (!name.startsWith(LOCATION + "/")) {
                    throw new FlywayException("Invalid reference migration resource: " + name);
                }
                ClassPathResource resource = new ClassPathResource(location, name, loader,
                        configuration.getEncoding(), "", configuration.isStream());
                if (!resource.exists()) {
                    throw new FlywayException("Missing embedded reference migration: " + name);
                }
                resources.add(resource);
            }
        } catch (IOException error) {
            throw new FlywayException("Cannot read reference migration manifest", error);
        }
        if (resources.isEmpty()) {
            throw new FlywayException("Empty reference migration manifest");
        }
        return resources;
    }
}
