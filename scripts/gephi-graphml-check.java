// Headless GraphML import check for the `export --graphml` output (#108).
//
// This is opt-in evidence, not a build dependency: repath neither ships nor
// requires Java or Gephi. Download the Gephi Toolkit (the GraphML import plugin
// the desktop application uses) once and run this file with the Java source
// launcher:
//
//   curl -L -o /tmp/gephi-toolkit.jar \
//     https://repo1.maven.org/maven2/org/gephi/gephi-toolkit/0.11.4/gephi-toolkit-0.11.4-all.jar
//   repath export --graphml --out /tmp/graph.graphml
//   java --add-opens java.base/java.net=ALL-UNNAMED -cp /tmp/gephi-toolkit.jar \
//     scripts/gephi-graphml-check.java /tmp/graph.graphml
//
// It prints the node/edge counts and the imported attribute types. Tested with
// Gephi Toolkit 0.11.4 and OpenJDK 25; the --add-opens silences a NetBeans
// Lookup warning that does not affect the import. Gephi's GUI was not available
// on the host that added this, so the import engine was exercised headlessly.

import java.io.File;

import org.gephi.graph.api.GraphController;
import org.gephi.graph.api.GraphModel;
import org.gephi.io.importer.api.Container;
import org.gephi.io.importer.api.ImportController;
import org.gephi.io.processor.plugin.DefaultProcessor;
import org.gephi.project.api.ProjectController;
import org.gephi.project.api.Workspace;
import org.openide.util.Lookup;

public class GephiGraphMLCheck {
    public static void main(String[] args) throws Exception {
        if (args.length != 1) {
            System.err.println("usage: java ... gephi-graphml-check.java <file.graphml>");
            System.exit(2);
        }
        ProjectController projects = Lookup.getDefault().lookup(ProjectController.class);
        projects.newProject();
        Workspace workspace = projects.getCurrentWorkspace();
        ImportController importer = Lookup.getDefault().lookup(ImportController.class);
        Container container = importer.importFile(new File(args[0]));
        if (container == null) {
            System.err.println("no GraphML importer accepted " + args[0]);
            System.exit(1);
        }
        importer.process(container, new DefaultProcessor(), workspace);
        GraphModel model = Lookup.getDefault().lookup(GraphController.class).getGraphModel(workspace);
        System.out.println("nodes=" + model.getGraph().getNodeCount()
                + " edges=" + model.getGraph().getEdgeCount());
        for (var column : model.getNodeTable()) {
            System.out.println("node-col " + column.getId() + " " + column.getTypeClass().getSimpleName());
        }
        for (var column : model.getEdgeTable()) {
            System.out.println("edge-col " + column.getId() + " " + column.getTypeClass().getSimpleName());
        }
    }
}
