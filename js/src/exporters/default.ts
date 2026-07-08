import type {
  ExportGenerationsRequest,
  ExportGenerationsResponse,
  ExportWorkflowStepsRequest,
  ExportWorkflowStepsResponse,
  GenerationExportConfig,
  GenerationExporter,
} from '../types.js';
import { HTTPGenerationExporter } from './http.js';

export function createDefaultGenerationExporter(config: GenerationExportConfig): GenerationExporter {
  switch (config.protocol) {
    case 'http':
      return new HTTPGenerationExporter(config.endpoint, config.headers);
    case 'grpc':
      return new LazyGRPCGenerationExporter(config.endpoint, config.headers, config.insecure);
    case 'none':
      return new NoopGenerationExporter();
    default:
      return new UnavailableGenerationExporter(
        new Error(`unsupported generation export protocol: ${config.protocol as string}`),
      );
  }
}

class NoopGenerationExporter implements GenerationExporter {
  async exportGenerations(request: ExportGenerationsRequest): Promise<ExportGenerationsResponse> {
    return {
      results: request.generations.map((generation) => ({
        generationId: generation.id,
        accepted: true,
      })),
    };
  }

  async exportWorkflowSteps(request: ExportWorkflowStepsRequest): Promise<ExportWorkflowStepsResponse> {
    return {
      results: request.workflowSteps.map((step) => ({
        stepId: step.id,
        accepted: true,
      })),
    };
  }
}

class UnavailableGenerationExporter implements GenerationExporter {
  constructor(private readonly reason: Error) {}

  async exportGenerations(): Promise<never> {
    throw this.reason;
  }

  async exportWorkflowSteps(): Promise<never> {
    throw this.reason;
  }
}

/**
 * Lazily loads the Node/gRPC exporter only when protocol=grpc is used.
 *
 * This keeps edge runtimes (for example Cloudflare Workers) on the HTTP/none
 * path from evaluating Node-only gRPC modules during startup.
 */
class LazyGRPCGenerationExporter implements GenerationExporter {
  private initPromise: Promise<GenerationExporter> | undefined;
  private exporter: GenerationExporter | undefined;

  constructor(
    private readonly endpoint: string,
    private readonly headers: Record<string, string> | undefined,
    private readonly insecure: boolean,
  ) {}

  async exportGenerations(request: ExportGenerationsRequest): Promise<ExportGenerationsResponse> {
    const exporter = await this.getExporter();
    return exporter.exportGenerations(request);
  }

  async exportWorkflowSteps(request: ExportWorkflowStepsRequest): Promise<ExportWorkflowStepsResponse> {
    const exporter = await this.getExporter();
    return exporter.exportWorkflowSteps(request);
  }

  async shutdown(): Promise<void> {
    if (this.initPromise === undefined && this.exporter === undefined) {
      return;
    }
    const exporter = await this.getExporter();
    await exporter.shutdown?.();
  }

  private async getExporter(): Promise<GenerationExporter> {
    if (this.exporter !== undefined) {
      return this.exporter;
    }
    if (this.initPromise !== undefined) {
      return this.initPromise;
    }
    this.initPromise = this.initializeExporter();
    this.exporter = await this.initPromise;
    return this.exporter;
  }

  private async initializeExporter(): Promise<GenerationExporter> {
    const grpc = await import('./grpc.js');
    return new grpc.GRPCGenerationExporter(this.endpoint, this.headers, this.insecure);
  }
}
