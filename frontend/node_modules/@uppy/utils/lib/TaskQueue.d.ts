/**
 * A promise that can be aborted.
 */
export interface AbortablePromise<T> extends Promise<T> {
    abort(reason?: unknown): void;
    /**
     * @deprecated Legacy compatibility - abort when signal fires
     */
    abortOn(signal?: AbortSignal): AbortablePromise<T>;
}
export interface TaskQueueOptions {
    concurrency?: number;
}
/**
 * A concurrent task queue with FIFO ordering.
 *
 * Tasks are functions that receive an AbortSignal and return a Promise.
 * The queue manages concurrency and processes tasks in insertion order.
 *
 * @example
 * ```ts
 * const queue = new TaskQueue({ concurrency: 3 })
 *
 * const promise = queue.add(async (signal) => {
 *   const response = await fetch(url, { signal })
 *   return response.json()
 * })
 *
 * // To abort:
 * promise.abort()
 * ```
 */
export declare class TaskQueue {
    #private;
    constructor(options?: TaskQueueOptions);
    /**
     * Add a task to the queue.
     *
     * @param task - Function receiving AbortSignal, returns Promise
     * @returns AbortablePromise that resolves with task result
     */
    add<T>(task: (signal: AbortSignal) => Promise<T>): AbortablePromise<T>;
    /**
     * Pause the queue. Running tasks continue, but no new tasks start.
     */
    pause(): void;
    /**
     * Resume the queue and start processing pending tasks.
     */
    resume(): void;
    /**
     * Clear all pending tasks from the queue.
     * Running tasks are not affected.
     *
     * @param reason - Optional reason for rejection (defaults to AbortError)
     */
    clear(reason?: unknown): void;
    get concurrency(): number;
    set concurrency(value: number);
    get pending(): number;
    get running(): number;
    get isPaused(): boolean;
    /**
     * @deprecated Legacy compatibility wrapper for RateLimitedQueue API.
     * Wraps a function so that when called, it's queued and returns an AbortablePromise.
     * Note: for legacy compatibility with RateLimitedQueue, the wrapped function
     * does not receive this queue's AbortSignal. Aborting the returned promise
     * will reject it, but it will not automatically cancel work inside the wrapped
     * function unless that function is wired to an external AbortSignal.
     */
    wrapPromiseFunction<T extends (...args: any[]) => Promise<any>>(fn: T): (...args: Parameters<T>) => AbortablePromise<Awaited<ReturnType<T>>>;
}
//# sourceMappingURL=TaskQueue.d.ts.map