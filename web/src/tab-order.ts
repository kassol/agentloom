export type DropEdge = "before" | "after";

export function reorderItem(items: string[], movingID: string, targetID: string, edge: DropEdge) {
  if (movingID === targetID || !items.includes(movingID) || !items.includes(targetID)) return items;
  const next = items.filter((id) => id !== movingID);
  const targetIndex = next.indexOf(targetID);
  next.splice(targetIndex + (edge === "after" ? 1 : 0), 0, movingID);
  return next.every((id, index) => id === items[index]) ? items : next;
}

export function moveItem(items: string[], movingID: string, offset: -1 | 1) {
  const index = items.indexOf(movingID);
  const targetIndex = index + offset;
  if (index < 0 || targetIndex < 0 || targetIndex >= items.length) return items;
  const next = [...items];
  [next[index], next[targetIndex]] = [next[targetIndex], next[index]];
  return next;
}
