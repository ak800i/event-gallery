import type { Body, Meta, Uppy } from '@uppy/core';
import { type StatusBarOptions } from '@uppy/status-bar';
import { Component } from 'react';
interface StatusBarProps<M extends Meta, B extends Body> extends StatusBarOptions {
    uppy: Uppy<M, B>;
}
/**
 * React component that renders a status bar containing upload progress and speed,
 * processing progress and pause/resume/cancel controls.
 */
declare class StatusBar<M extends Meta, B extends Body> extends Component<StatusBarProps<M, B>> {
    private container;
    private plugin;
    componentDidMount(): void;
    componentDidUpdate(prevProps: StatusBar<M, B>['props']): void;
    componentWillUnmount(): void;
    installPlugin(): void;
    uninstallPlugin(props?: Readonly<StatusBarProps<M, B>>): void;
    render(): import("react").DetailedReactHTMLElement<{
        className: string;
        ref: (container: HTMLElement) => void;
    }, HTMLElement>;
}
export default StatusBar;
//# sourceMappingURL=StatusBar.d.ts.map