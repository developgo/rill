import { fetchAllFileNames } from "@rilldata/web-common/features/entity-management/file-selectors";
import { getName } from "@rilldata/web-common/features/entity-management/name-utils";
import { ResourceKind } from "@rilldata/web-common/features/entity-management/resource-selectors";
import { queryClient } from "@rilldata/web-common/lib/svelte-query/globalQueryClient";
import { runtimeServicePutFile } from "@rilldata/web-common/runtime-client";
import { runtime } from "@rilldata/web-common/runtime-client/runtime-store";
import { get } from "svelte/store";

export async function handleEntityCreate(kind: ResourceKind) {
  if (!(kind in ResourceKindMap)) return;
  const instanceId = get(runtime).instanceId;
  const allNames = await fetchAllFileNames(queryClient, instanceId);
  const { name, folder, baseContent, extension } = ResourceKindMap[kind];
  const newName = getName(name, allNames);

  const newPath = `${folder ?? name + "s"}/${newName}${extension ?? ".yaml"}`;

  await runtimeServicePutFile(instanceId, newPath, {
    blob: baseContent,
    create: true,
    createOnly: true,
  });
  return `/files//${newPath}`;
}

const ResourceKindMap: Record<
  ResourceKind,
  {
    name: string;
    folder?: string; // adds "s" to name by default
    baseContent: string;
    extension?: string;
  }
> = {
  [ResourceKind.ProjectParser]: { baseContent: "", name: "" },
  [ResourceKind.Source]: {
    name: "source",
    baseContent: "",
  },
  [ResourceKind.Model]: {
    name: "model",
    extension: ".sql",
    baseContent: `-- @kind: model
select ...
`,
  },
  [ResourceKind.MetricsView]: {
    name: "dashboard",
    baseContent: `kind: metrics_view

`,
  },
  [ResourceKind.API]: {
    name: "api",
    baseContent: `kind: api

sql:
  select ...
`,
  },
  [ResourceKind.Chart]: {
    name: "chart",
    baseContent: `kind: chart
data:
  metrics_sql: |
    SELECT advertiser_name, AGGREGATE(measure_2)
    FROM Bids_Sample_Dash
    GROUP BY advertiser_name
    ORDER BY measure_2 DESC
    LIMIT 20

vega_lite: |
  {
    "$schema": "https://vega.github.io/schema/vega-lite/v5.json",
    "data": {"name": "table"},
    "mark": "bar",
    "width": "container",
    "encoding": {
      "x": {"field": "advertiser_name", "type": "nominal"},
      "y": {"field": "measure_2", "type": "quantitative"}
    }
  }`,
  },
  [ResourceKind.Dashboard]: {
    name: "custom-dashboard",
    baseContent: `kind: dashboard
columns: 10
gap: 2`,
  },
  [ResourceKind.Theme]: {
    name: "theme",
    baseContent: `kind: theme
colors:
  primary: crimson 
  secondary: lime 
`,
  },
  [ResourceKind.Report]: {
    name: "report",
    baseContent: `kind: report

...
`,
  },
  [ResourceKind.Alert]: {
    name: "alert",
    baseContent: `kind: alert

...
`,
  },
};
