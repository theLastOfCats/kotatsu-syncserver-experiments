package reference.nativeimage;

import com.oracle.svm.core.annotate.Substitute;
import com.oracle.svm.core.annotate.TargetClass;
import java.lang.annotation.Annotation;
import java.lang.reflect.Proxy;
import kotlin.jvm.JvmClassMappingKt;
import kotlin.jvm.JvmInline;
import kotlin.jvm.internal.Intrinsics;
import kotlin.reflect.KClass;

@TargetClass(className = "org.ktorm.entity.ReflectionsKt")
final class Target_org_ktorm_entity_ReflectionsKt {
    @Substitute
    public static boolean isInline(KClass<?> type) {
        Intrinsics.checkNotNullParameter(type, "<this>");
        // Kotlin reflection cannot resolve Graal's generated proxy names. A JDK
        // proxy is never a Kotlin value class, so no unboxing is applicable.
        if (Proxy.isProxyClass(JvmClassMappingKt.getJavaClass(type))) {
            return false;
        }
        if (!type.isValue()) {
            return false;
        }
        for (Annotation annotation : type.getAnnotations()) {
            if (annotation instanceof JvmInline) {
                return true;
            }
        }
        return false;
    }
}
