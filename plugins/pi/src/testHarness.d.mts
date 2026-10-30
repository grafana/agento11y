export function assertSdkBuilt(packageDir: string): void;
export function copyTestPackage(packageDir: string, root: string): string;
export function testEnvironment(
  root: string,
  name: string,
  path: string,
): NodeJS.ProcessEnv;
export function runTestProcess(options: {
  name: string;
  command: string;
  args: string[];
  cwd: string;
  env: NodeJS.ProcessEnv;
  timeout: number;
  stdio?: "pipe" | "inherit";
}): Promise<{ stdout: string; stderr: string }>;
