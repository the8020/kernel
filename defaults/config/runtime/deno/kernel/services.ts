// Resolved package-service indexing contract. Durations are JSON nanoseconds.
// Application declarations, defaults, overrides, and storage are not kernel input.
export interface ServiceConfiguration {
  execution: { anonymous_user: string };
  lifecycle: {
    service_type: "stateless" | "session";
    session_keep_alive: number;
  };
  scaling: {
    minimum_workers: number;
    maximum_workers: number;
    concurrency_per_worker: number;
    target_utilization: number;
    worker_keep_alive: number;
  };
  placement: {
    sandbox_group: string;
    minimum_sandboxes: number;
    workers_per_sandbox: number;
  };
  timeouts: { request: number; drain: number; idle: number };
}

export interface ServiceSpecification {
  service_id: string;
  version: number;
  code_revision: string;
  entrypoint: string;
  enabled: boolean;
  description: string;
  openapi: { title: string; version: string; description: string };
  access: {
    mode: "public" | "authenticated";
    unauthenticated: {
      action: "reject" | "redirect";
      status: number;
      message: string;
      redirect_url: string;
    };
  };
  configuration: ServiceConfiguration;
}

export interface ServiceIndexScope {
  readonly package_id: string;
  readonly package_commit: string;
  readonly active: boolean;
}

export interface ServiceIndexState {
  services: ServiceSpecification[];
}
