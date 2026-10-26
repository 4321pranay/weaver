package weaver;

import java.util.*;
import java.util.stream.*;
import sootup.core.jimple.basic.Value;
import sootup.core.jimple.common.expr.JInterfaceInvokeExpr;
import sootup.core.jimple.common.stmt.Stmt;
import sootup.core.model.SourceType;
import sootup.core.types.ClassType;
import sootup.java.bytecode.frontend.inputlocation.JavaClassPathAnalysisInputLocation;
import sootup.java.core.AnnotationUsage;
import sootup.java.core.JavaSootClass;
import sootup.java.core.JavaSootMethod;
import sootup.java.core.views.JavaView;

/**
 * Interface call sites and the concrete methods they reach, for weaver's entries
 * walk. Spring model: an implementation counts only if it is a bean (a stereotype
 * annotation); among several beans one marked @Primary wins. Without any bean
 * implementation (plain Java), every implementation counts (CHA).
 * Prints one JSON object per line: {"caller", "line", "targets": ["pkg/Impl#m("]}.
 * usage: java weaver.JavaCG <classpath of the service's compiled classes>
 */
public class JavaCG {
  static final Set<String> BEANS = Set.of("Component", "Service", "Repository", "Controller",
      "RestController", "Configuration");

  public static void main(String[] args) {
    JavaView view = new JavaView(List.of(new JavaClassPathAnalysisInputLocation(args[0], SourceType.Application)));
    var hier = view.getTypeHierarchy();
    Map<ClassType, JavaSootClass> classes = view.getClasses().collect(Collectors.toMap(JavaSootClass::getType, c -> c, (a, b) -> a));
    StringBuilder out = new StringBuilder();
    for (JavaSootClass c : classes.values()) {
      for (JavaSootMethod m : c.getMethods()) {
        if (!m.hasBody()) continue;
        for (Stmt s : m.getBody().getStmts()) {
          for (Value v : (Iterable<Value>) s.getUses()::iterator) {
            if (!(v instanceof JInterfaceInvokeExpr ie)) continue;
            var sig = ie.getMethodSignature();
            ClassType iface = sig.getDeclClassType();
            List<JavaSootClass> impls;
            try {
              impls = hier.implementersOf(iface).map(classes::get).filter(Objects::nonNull)
                  .filter(k -> !k.isInterface() && !k.isAbstract()).collect(Collectors.toList());
            } catch (RuntimeException e) {
              continue; // interface outside the analysed classes (a library)
            }
            if (impls.isEmpty()) continue;
            List<JavaSootClass> beans = impls.stream().filter(k -> has(k, BEANS)).collect(Collectors.toList());
            List<JavaSootClass> pick = beans.isEmpty() ? impls : beans;
            if (pick.size() > 1) {
              List<JavaSootClass> primary = pick.stream().filter(k -> has(k, Set.of("Primary"))).collect(Collectors.toList());
              if (primary.size() == 1) pick = primary;
            }
            int line = s.getPositionInfo().getStmtPosition().getFirstLine();
            String targets = pick.stream()
                .map(k -> "\"" + desc(k.getType()) + "#" + sig.getName() + "(\"")
                .collect(Collectors.joining(","));
            out.append("{\"caller\":\"").append(desc(c.getType())).append("#").append(m.getName())
                .append("(\",\"line\":").append(line)
                .append(",\"model\":\"").append(beans.isEmpty() ? "cha" : "spring").append("\"")
                .append(",\"targets\":[").append(targets).append("]}\n");
          }
        }
      }
    }
    System.out.print(out);
  }

  static boolean has(JavaSootClass k, Set<String> names) {
    for (AnnotationUsage a : k.getAnnotations()) {
      String n = a.getAnnotation().getFullyQualifiedName();
      if (names.contains(n.substring(n.lastIndexOf('.') + 1))) return true;
    }
    return false;
  }

  // com.shop.A$Inner -> com/shop/A#Inner (SCIP's descriptor form, without the trailing #)
  static String desc(ClassType t) {
    return t.getFullyQualifiedName().replace('.', '/').replace('$', '#');
  }
}
