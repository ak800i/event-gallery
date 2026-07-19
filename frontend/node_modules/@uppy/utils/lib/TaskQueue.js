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
export class TaskQueue {
    #queue = [];
    #running = 0;
    #concurrency;
    #paused = false;
    constructor(options) {
        const limit = options?.concurrency;
        this.#concurrency =
            typeof limit !== 'number' || limit === 0 ? Infinity : limit;
    }
    /**
     * Add a task to the queue.
     *
     * @param task - Function receiving AbortSignal, returns Promise
     * @returns AbortablePromise that resolves with task result
     */
    add(task) {
        const controller = new AbortController();
        let resolve;
        let reject;
        const promise = new Promise((res, rej) => {
            resolve = res;
            reject = rej;
        });
        const queuedTask = {
            run: () => task(controller.signal),
            resolve,
            reject,
            controller,
        };
        // Handle abort while queued
        controller.signal.addEventListener('abort', () => {
            const index = this.#queue.indexOf(queuedTask);
            if (index !== -1) {
                this.#queue.splice(index, 1);
                reject(controller.signal.reason ??
                    new DOMException('Aborted', 'AbortError'));
            }
        }, { once: true });
        promise.abort = (reason) => {
            controller.abort(reason ?? new DOMException('Aborted', 'AbortError'));
        };
        // Legacy compatibility: abortOn method
        promise.abortOn = (signal) => {
            if (signal) {
                const onAbort = () => promise.abort(signal.reason);
                signal.addEventListener('abort', onAbort, { once: true });
                promise.then(() => signal.removeEventListener('abort', onAbort), () => signal.removeEventListener('abort', onAbort));
            }
            return promise;
        };
        // Run immediately or queue
        if (!this.#paused && this.#running < this.#concurrency) {
            this.#execute(queuedTask);
        }
        else {
            this.#queue.push(queuedTask);
        }
        return promise;
    }
    #execute(task) {
        this.#running++;
        // Check if already aborted before starting
        if (task.controller.signal.aborted) {
            this.#running--;
            task.reject(task.controller.signal.reason ??
                new DOMException('Aborted', 'AbortError'));
            this.#advance();
            return;
        }
        let runPromise;
        try {
            runPromise = task.run();
        }
        catch (error) {
            runPromise = Promise.reject(error);
        }
        runPromise
            .then((result) => {
            if (task.controller.signal.aborted) {
                task.reject(task.controller.signal.reason ??
                    new DOMException('Aborted', 'AbortError'));
            }
            else {
                task.resolve(result);
            }
        }, (error) => {
            task.reject(error);
        })
            .finally(() => {
            this.#running--;
            this.#advance();
        });
    }
    #advance() {
        // Use microtask to allow batch aborts without starting new tasks
        queueMicrotask(() => {
            if (this.#paused || this.#running >= this.#concurrency)
                return;
            while (this.#queue.length > 0) {
                const next = this.#queue.shift();
                if (next.controller.signal.aborted)
                    continue;
                this.#execute(next);
                return;
            }
        });
    }
    /**
     * Pause the queue. Running tasks continue, but no new tasks start.
     */
    pause() {
        this.#paused = true;
    }
    /**
     * Resume the queue and start processing pending tasks.
     */
    resume() {
        this.#paused = false;
        // Kick off tasks up to concurrency limit
        const available = this.#concurrency - this.#running;
        for (let i = 0; i < available; i++) {
            this.#advance();
        }
    }
    /**
     * Clear all pending tasks from the queue.
     * Running tasks are not affected.
     *
     * @param reason - Optional reason for rejection (defaults to AbortError)
     */
    clear(reason) {
        const tasks = this.#queue.splice(0);
        const error = reason ?? new DOMException('Cleared', 'AbortError');
        for (const task of tasks) {
            task.controller.abort(error);
            task.reject(error);
        }
    }
    get concurrency() {
        return this.#concurrency;
    }
    set concurrency(value) {
        this.#concurrency =
            typeof value !== 'number' || value === 0 ? Infinity : value;
        // If concurrency increased, try to start more tasks
        if (!this.#paused) {
            const available = this.#concurrency - this.#running;
            for (let i = 0; i < available; i++) {
                this.#advance();
            }
        }
    }
    get pending() {
        return this.#queue.length;
    }
    get running() {
        return this.#running;
    }
    get isPaused() {
        return this.#paused;
    }
    /**
     * @deprecated Legacy compatibility wrapper for RateLimitedQueue API.
     * Wraps a function so that when called, it's queued and returns an AbortablePromise.
     * Note: for legacy compatibility with RateLimitedQueue, the wrapped function
     * does not receive this queue's AbortSignal. Aborting the returned promise
     * will reject it, but it will not automatically cancel work inside the wrapped
     * function unless that function is wired to an external AbortSignal.
     */
    wrapPromiseFunction(fn) {
        return (...args) => {
            return this.add((signal) => {
                // The wrapped function doesn't receive signal directly,
                // caller is responsible for using signal if needed
                void signal;
                return fn(...args);
            });
        };
    }
}
