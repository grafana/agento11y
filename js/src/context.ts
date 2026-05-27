type SigilContextValues = {
  conversationId?: string;
  conversationTitle?: string;
  userId?: string;
  agentName?: string;
  agentVersion?: string;
};

type ContextStorage<T> = {
  getStore(): T | undefined;
  run<R>(store: T, callback: () => R): R;
};

type AsyncLocalStorageConstructor = new <T>() => ContextStorage<T>;

export function withConversationId<T>(conversationId: string, callback: () => T): T {
  return runWithContext({ conversationId }, callback);
}

export function withConversationTitle<T>(conversationTitle: string, callback: () => T): T {
  return runWithContext({ conversationTitle }, callback);
}

export function withUserId<T>(userId: string, callback: () => T): T {
  return runWithContext({ userId }, callback);
}

export function withAgentName<T>(agentName: string, callback: () => T): T {
  return runWithContext({ agentName }, callback);
}

export function withAgentVersion<T>(agentVersion: string, callback: () => T): T {
  return runWithContext({ agentVersion }, callback);
}

export function conversationIdFromContext(): string | undefined {
  return normalizedString(storage.getStore()?.conversationId);
}

export function conversationTitleFromContext(): string | undefined {
  return normalizedString(storage.getStore()?.conversationTitle);
}

export function userIdFromContext(): string | undefined {
  return normalizedString(storage.getStore()?.userId);
}

export function agentNameFromContext(): string | undefined {
  return normalizedString(storage.getStore()?.agentName);
}

export function agentVersionFromContext(): string | undefined {
  return normalizedString(storage.getStore()?.agentVersion);
}

function runWithContext<T>(nextValues: SigilContextValues, callback: () => T): T {
  const currentValues = storage.getStore() ?? {};
  const mergedValues: SigilContextValues = { ...currentValues };

  for (const [key, value] of Object.entries(nextValues)) {
    const normalized = normalizedString(value);
    if (normalized === undefined) {
      delete mergedValues[key as keyof SigilContextValues];
      continue;
    }
    mergedValues[key as keyof SigilContextValues] = normalized;
  }

  return storage.run(mergedValues, callback);
}

function normalizedString(value: string | undefined): string | undefined {
  if (value === undefined) {
    return undefined;
  }
  const trimmed = value.trim();
  return trimmed.length > 0 ? trimmed : undefined;
}

function resolveNodeAsyncLocalStorage(): AsyncLocalStorageConstructor | undefined {
  const processWithBuiltins = (globalThis as { process?: { getBuiltinModule?: (id: string) => unknown } }).process;
  const module = processWithBuiltins?.getBuiltinModule?.('async_hooks') as
    | { AsyncLocalStorage?: AsyncLocalStorageConstructor }
    | undefined;
  return module?.AsyncLocalStorage;
}

class FallbackContextStorage<T> implements ContextStorage<T> {
  private current: T | undefined;

  getStore(): T | undefined {
    return this.current;
  }

  run<R>(store: T, callback: () => R): R {
    const previous = this.current;
    this.current = store;

    try {
      const result = callback();
      if (isPromiseLike(result)) {
        return result.finally(() => {
          this.current = previous;
        }) as R;
      }
      this.current = previous;
      return result;
    } catch (error) {
      this.current = previous;
      throw error;
    }
  }
}

function isPromiseLike(value: unknown): value is Promise<unknown> {
  return (
    typeof value === 'object' &&
    value !== null &&
    'finally' in value &&
    typeof (value as { finally?: unknown }).finally === 'function'
  );
}

const AsyncLocalStorage = resolveNodeAsyncLocalStorage();
const storage: ContextStorage<SigilContextValues> =
  AsyncLocalStorage !== undefined
    ? new AsyncLocalStorage<SigilContextValues>()
    : new FallbackContextStorage<SigilContextValues>();
