import type { DeleteTaskStatus, GroupKind } from "../../api/contracts";

export interface DeleteReviewMember {
  readonly fileId: number;
  readonly machineId: string;
  readonly path: string;
  /** 字节大小；用于选择摘要汇总，来源不可知时缺省。 */
  readonly size?: number;
}

export interface DeleteReviewSnapshot {
  readonly groupId: number;
  readonly kind: GroupKind;
  readonly scopeKey: string;
  readonly members: readonly DeleteReviewMember[];
  readonly terminalStatus?: DeleteTaskStatus;
  readonly reconciled?: boolean;
}

export interface DeleteRetryPlan {
  readonly hasIssues: boolean;
  readonly retryMembers: readonly DeleteReviewMember[];
}

function memberKey(machineId: string, path: string) {
  return JSON.stringify([machineId, path]);
}

export function deriveDeleteRetryPlan(
  status: DeleteTaskStatus,
  snapshot: DeleteReviewSnapshot
): DeleteRetryPlan {
  const hasIssues = status.failed > 0 || status.uncertain > 0 || status.stateSyncFailures > 0;
  if (!hasIssues) return { hasIssues, retryMembers: [] };
  const snapshotByKey = new Map(snapshot.members.map(member => [
    memberKey(member.machineId, member.path),
    member
  ]));
  const mappedProblems = status.problems
    .map(problem => snapshotByKey.get(memberKey(problem.machineId, problem.path)));
  const minimumProblemItems = Math.max(
    status.failed,
    status.uncertain,
    status.stateSyncFailures
  );
  const uniqueMappedMembers = [...new Map(mappedProblems
    .filter((member): member is DeleteReviewMember => member !== undefined)
    .map(member => [member.fileId, member])).values()];
  const mappingComplete = mappedProblems.every(member => member !== undefined) &&
    uniqueMappedMembers.length >= minimumProblemItems;
  const retryMembers = mappingComplete
    ? uniqueMappedMembers
    : snapshot.members;
  return { hasIssues, retryMembers };
}
