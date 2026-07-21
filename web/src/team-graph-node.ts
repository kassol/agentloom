import { Position, type Node } from "@xyflow/react";
import { TEAM_GRAPH_NODE_HEIGHT, TEAM_GRAPH_NODE_WIDTH } from "./team-graph-layout";

const TEAM_GRAPH_HANDLE_SIZE = 1;

export function stableTeamGraphNodeGeometry(): Pick<Node, "width" | "height" | "handles"> {
  const handleY = (TEAM_GRAPH_NODE_HEIGHT - TEAM_GRAPH_HANDLE_SIZE) / 2;

  // Fixed geometry lets React Flow place edges before ResizeObserver catches up.
  return {
    width: TEAM_GRAPH_NODE_WIDTH,
    height: TEAM_GRAPH_NODE_HEIGHT,
    handles: [
      {
        type: "target",
        position: Position.Left,
        x: 0,
        y: handleY,
        width: TEAM_GRAPH_HANDLE_SIZE,
        height: TEAM_GRAPH_HANDLE_SIZE,
      },
      {
        type: "source",
        position: Position.Right,
        x: TEAM_GRAPH_NODE_WIDTH - TEAM_GRAPH_HANDLE_SIZE,
        y: handleY,
        width: TEAM_GRAPH_HANDLE_SIZE,
        height: TEAM_GRAPH_HANDLE_SIZE,
      },
    ],
  };
}
