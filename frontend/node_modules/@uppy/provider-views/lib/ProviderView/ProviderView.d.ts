import type { Body, Meta, PartialTree, PartialTreeFile, PartialTreeFolder, PartialTreeFolderNode, PartialTreeId, UnknownProviderPlugin } from '@uppy/core';
import type { CompanionFile, I18n } from '@uppy/utils';
import type { h } from 'preact';
export declare function defaultPickerIcon(): h.JSX.Element;
type Optional<T, K extends keyof T> = Pick<Partial<T>, K> & Omit<T, K>;
export interface Opts<M extends Meta, B extends Body> {
    provider: UnknownProviderPlugin<M, B>['provider'];
    viewType: 'list' | 'grid';
    showTitles: boolean;
    showFilter: boolean;
    showBreadcrumbs: boolean;
    loadAllFiles: boolean;
    renderAuthForm?: (args: {
        pluginName: string;
        i18n: I18n;
        loading: boolean | string;
        onAuth: (authFormData: unknown) => Promise<void>;
    }) => h.JSX.Element;
    virtualList: boolean;
    supportsSearch?: boolean;
}
type PassedOpts<M extends Meta, B extends Body> = Optional<Opts<M, B>, 'viewType' | 'showTitles' | 'showFilter' | 'showBreadcrumbs' | 'loadAllFiles' | 'virtualList'>;
type RenderOpts<M extends Meta, B extends Body> = Omit<PassedOpts<M, B>, 'provider'>;
/**
 * Class to easily generate generic views for Provider plugins
 *
 * We have a *search view* and a *normal view*.
 * Search view is only used when the Provider supports server side search i.e. provider.search method is implemented for the provider.
 * The state is stored in searchResults.
 * Search view is implemented in components GlobalSearchView and SearchResultItem.
 * We conditionally switch between search view and normal in the render method when a server side search is initiated.
 * When users type their search query in search input box (SearchInput component), we debounce the input and call provider.search method to fetch results from the server.
 * when the user enters a folder in search results or clears the search input query we switch back to Normal View.
 */
export default class ProviderView<M extends Meta, B extends Body> {
    #private;
    static VERSION: string;
    plugin: UnknownProviderPlugin<M, B>;
    provider: UnknownProviderPlugin<M, B>['provider'];
    opts: Opts<M, B>;
    isHandlingScroll: boolean;
    previousCheckbox: string | null;
    constructor(plugin: UnknownProviderPlugin<M, B>, opts: PassedOpts<M, B>);
    resetPluginState(): void;
    tearDown(): void;
    setLoading(loading: boolean | string): void;
    get isLoading(): string | boolean;
    cancelSelection(): void;
    clearSearchState(): void;
    onSearchInput: (s: string) => void;
    openSearchResultFolder(folderId: PartialTreeId): Promise<void>;
    openFolder(folderId: PartialTreeId): Promise<void>;
    /**
     * Removes session token on client side.
     */
    logout(): Promise<void>;
    handleAuth(authFormData?: unknown): Promise<void>;
    handleScroll(event: Event): Promise<void>;
    validateSingleFile: (file: CompanionFile) => string | null;
    donePicking(): Promise<void>;
    toggleCheckbox(ourItem: PartialTreeFolderNode | PartialTreeFile, isShiftKeyPressed: boolean): void;
    getDisplayedPartialTree: () => (PartialTreeFile | PartialTreeFolderNode)[];
    getBreadcrumbs: () => PartialTreeFolder[];
    getSelectedAmount: () => number;
    validateAggregateRestrictions: (partialTree: PartialTree) => string | null;
    render(state: unknown, viewOptions?: RenderOpts<M, B>): h.JSX.Element;
}
export {};
//# sourceMappingURL=ProviderView.d.ts.map