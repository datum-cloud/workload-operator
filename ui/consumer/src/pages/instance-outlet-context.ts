/**
 * Instance tab bodies share this context from the layout shell so Overview,
 * Logs, and Metrics do not re-fetch or remount breadcrumbs / title / tabs.
 */
import type { Instance } from '../schema';
import { useOutletContext } from 'react-router';

export type InstanceOutletContext = {
  instance: Instance;
  workloadName?: string;
  projectId?: string;
  logsHref: string;
  metricsHref: string;
  /** HTTPProxy name when the workload is published on a URL; otherwise unset. */
  proxyId?: string;
};

export function useInstanceOutlet(): InstanceOutletContext {
  return useOutletContext<InstanceOutletContext>();
}
