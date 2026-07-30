import JobsList from "./JobsList";

/**
 * The admin job log: provisioning runs and anything else that is not a variant
 * annotation.
 *
 * Separate from Results because the two answer different questions. Results is
 * "what did I annotate"; this is "is the deployment healthy" — a failed download
 * is an operator's problem, not a user's, and mixing them buries both.
 */
export default function SystemJobs() {
  return (
    <JobsList
      kind="download"
      title="System jobs"
      lede="Provisioning runs. These fetch source data; they produce no variants."
    />
  );
}
