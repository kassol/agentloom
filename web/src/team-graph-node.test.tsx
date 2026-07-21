import { Handle, Position, ReactFlow, type NodeProps } from "@xyflow/react";
import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { TEAM_GRAPH_NODE_HEIGHT, TEAM_GRAPH_NODE_WIDTH } from "./team-graph-layout";
import { stableTeamGraphNodeGeometry } from "./team-graph-node";

function TestNode(_: NodeProps) {
  return (
    <div>
      <Handle type="target" position={Position.Left} />
      <Handle type="source" position={Position.Right} />
    </div>
  );
}

describe("stable team graph node geometry", () => {
  it("renders edges without waiting for ResizeObserver measurement", () => {
    const originalResizeObserver = globalThis.ResizeObserver;
    class StalledResizeObserver {
      observe() {}
      unobserve() {}
      disconnect() {}
    }
    Object.defineProperty(globalThis, "ResizeObserver", { configurable: true, value: StalledResizeObserver });

    try {
      const nodes = [
        { id: "source", type: "test", position: { x: 0, y: 0 }, data: {}, ...stableTeamGraphNodeGeometry() },
        { id: "target", type: "test", position: { x: 320, y: 0 }, data: {}, ...stableTeamGraphNodeGeometry() },
      ];
      const { container } = render(
        <div style={{ width: 800, height: 400 }}>
          <ReactFlow nodes={nodes} edges={[{ id: "edge", source: "source", target: "target" }]} nodeTypes={{ test: TestNode }} />
        </div>,
      );

      expect(container.querySelectorAll(".react-flow__edge")).toHaveLength(1);
      expect(container.querySelector(".react-flow__edge-path")).toBeInTheDocument();
    } finally {
      Object.defineProperty(globalThis, "ResizeObserver", { configurable: true, value: originalResizeObserver });
    }
  });

  it("declares the same fixed dimensions used by the graph layout", () => {
    const geometry = stableTeamGraphNodeGeometry();
    expect(geometry.width).toBe(TEAM_GRAPH_NODE_WIDTH);
    expect(geometry.height).toBe(TEAM_GRAPH_NODE_HEIGHT);
    expect(geometry.handles).toHaveLength(2);
  });
});
