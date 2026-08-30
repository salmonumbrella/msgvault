import type { components } from '../api/generated/schema';
import type { ExploreURLState } from '../explore/models';

export type OperationAction = components['schemas']['OperationLaneStatus']['supported_actions'][number];
export type OperationActionOutcome = 'succeeded' | 'conflict' | 'failed' | 'discarded';
export type OperationKind = components['schemas']['OperationRunSummary']['kind'];
export type OperationLane = components['schemas']['OperationRunSummary']['lane'];
export type OperationLaneStatus = components['schemas']['OperationLaneStatus'];
export type OperationRunDetail = components['schemas']['OperationRunDetail'];
export type OperationRunSummary = components['schemas']['OperationRunSummary'];
export type OperationRunsResponse = components['schemas']['OperationRunsResponse'];
export type OperationStatusResponse = components['schemas']['OperationStatusResponse'];
export type OperationState = components['schemas']['OperationRunSummary']['state'];
export type OperationUnavailableKind = components['schemas']['OperationUnavailableKind'];

export type OperationsURLState = Pick<ExploreURLState,
  | 'operationLane'
  | 'operationKind'
  | 'operationState'
  | 'operationStartedFrom'
  | 'operationStartedBefore'
  | 'operationRunID'
  | 'operationStatus'>;

export interface OperationStatusLane {
  readonly lane: OperationLane;
  readonly kinds: readonly OperationLaneStatus[];
}

export interface OperationsSnapshot {
  readonly statusLanes: readonly OperationStatusLane[];
  readonly rows: readonly OperationRunSummary[];
  readonly unavailableKinds: readonly OperationUnavailableKind[];
  readonly detail: OperationRunDetail | null;
  readonly membershipRevision: number | null;
  readonly nextCursor: string | null;
  readonly statusReadable: boolean;
  readonly historyReadable: boolean;
  readonly initialLoading: boolean;
  readonly backgroundLoading: boolean;
  readonly paging: boolean;
  readonly detailLoading: boolean;
  readonly statusError: string | null;
  readonly runsError: string | null;
  readonly detailError: string | null;
  readonly conflict: string | null;
  readonly restartRequired: boolean;
  readonly actionPending: OperationAction | null;
  readonly actionConflict: string | null;
  readonly actionError: string | null;
}
