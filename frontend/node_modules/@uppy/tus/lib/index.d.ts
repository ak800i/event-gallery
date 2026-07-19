import type { Body, DefinePluginOpts, Meta, PluginOpts, Uppy, UppyFile } from '@uppy/core';
import { BasePlugin, EventManager } from '@uppy/core';
import { RateLimitedQueue } from '@uppy/utils';
import * as tus from 'tus-js-client';
type RestTusUploadOptions = Omit<tus.UploadOptions, 'onShouldRetry' | 'onBeforeRequest' | 'headers'>;
export type TusDetailedError = tus.DetailedError;
export type TusBody = {
    xhr: XMLHttpRequest;
};
export interface TusOpts<M extends Meta, B extends Body> extends PluginOpts, RestTusUploadOptions {
    endpoint?: string;
    headers?: Record<string, string> | ((file: UppyFile<M, B>) => Record<string, string>);
    limit?: number;
    chunkSize?: number;
    onBeforeRequest?: (req: tus.HttpRequest, file: UppyFile<M, B>) => void | Promise<void>;
    onShouldRetry?: (err: tus.DetailedError, retryAttempt: number, options: TusOpts<M, B>, next: (e: tus.DetailedError) => boolean) => boolean;
    retryDelays?: number[];
    withCredentials?: boolean;
    allowedMetaFields?: boolean | string[];
    rateLimitedQueue?: RateLimitedQueue;
}
export type { TusOpts as TusOptions };
declare const defaultOptions: {
    limit: number;
    retryDelays: number[];
    withCredentials: false;
    allowedMetaFields: true;
};
type Opts<M extends Meta, B extends Body> = DefinePluginOpts<TusOpts<M, B>, keyof typeof defaultOptions>;
declare module '@uppy/utils' {
    interface LocalUppyFile<M extends Meta, B extends Body> {
        tus?: TusOpts<M, B>;
    }
    interface RemoteUppyFile<M extends Meta, B extends Body> {
        tus?: TusOpts<M, B>;
    }
}
declare module '@uppy/core' {
    interface PluginTypeRegistry<M extends Meta, B extends Body> {
        Tus: Tus<M, B>;
    }
}
/**
 * Tus resumable file uploader
 */
export default class Tus<M extends Meta, B extends Body> extends BasePlugin<Opts<M, B>, M, B> {
    #private;
    static VERSION: string;
    requests: RateLimitedQueue;
    uploaders: Record<string, tus.Upload | null>;
    uploaderEvents: Record<string, EventManager<M, B> | null>;
    constructor(uppy: Uppy<M, B>, opts: TusOpts<M, B>);
    /**
     * Clean up all references for a file's upload: the tus.Upload instance,
     * any events related to the file, and the Companion WebSocket connection.
     */
    resetUploaderReferences(fileID: string, opts?: {
        abort: boolean;
    }): void;
    /**
     * Store the uploadUrl on the file options, so that when Golden Retriever
     * restores state, we will continue uploading to the correct URL.
     */
    onReceiveUploadUrl(file: UppyFile<M, B>, uploadURL: string | null): void;
    install(): void;
    uninstall(): void;
}
//# sourceMappingURL=index.d.ts.map