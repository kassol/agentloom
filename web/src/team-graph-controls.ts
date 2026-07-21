export const EDGE_CONTROL_SIZE_TOLERANCE = 0.5;

export function edgeControlMeetsMinimum(
  width: number,
  height: number,
  minimumSize: number,
  tolerance = EDGE_CONTROL_SIZE_TOLERANCE,
) {
  return width + tolerance >= minimumSize && height + tolerance >= minimumSize;
}
