import { jsx as _jsx, jsxs as _jsxs } from "preact/jsx-runtime";
import { remoteFileObjToLocal } from '@uppy/utils';
import classNames from 'classnames';
import debounce from 'lodash/debounce.js';
import packageJson from '../../package.json' with { type: 'json' };
import Browser from '../Browser.js';
import FilterInput from '../FilterInput.js';
import FooterActions from '../FooterActions.js';
import addFiles from '../utils/addFiles.js';
import getClickedRange from '../utils/getClickedRange.js';
import handleError from '../utils/handleError.js';
import getBreadcrumbs from '../utils/PartialTreeUtils/getBreadcrumbs.js';
import getCheckedFilesWithPaths from '../utils/PartialTreeUtils/getCheckedFilesWithPaths.js';
import getNumberOfSelectedFiles from '../utils/PartialTreeUtils/getNumberOfSelectedFiles.js';
import PartialTreeUtils from '../utils/PartialTreeUtils/index.js';
import shouldHandleScroll from '../utils/shouldHandleScroll.js';
import AuthView from './AuthView.js';
import GlobalSearchView from './GlobalSearchView.js';
import Header from './Header.js';
export function defaultPickerIcon() {
    return (_jsx("svg", { "aria-hidden": "true", focusable: "false", width: "30", height: "30", viewBox: "0 0 30 30", children: _jsx("path", { d: "M15 30c8.284 0 15-6.716 15-15 0-8.284-6.716-15-15-15C6.716 0 0 6.716 0 15c0 8.284 6.716 15 15 15zm4.258-12.676v6.846h-8.426v-6.846H5.204l9.82-12.364 9.82 12.364H19.26z" }) }));
}
const getDefaultState = (rootFolderId) => ({
    authenticated: undefined, // we don't know yet
    partialTree: [
        {
            type: 'root',
            id: rootFolderId,
            cached: false,
            nextPagePath: null,
        },
    ],
    currentFolderId: rootFolderId,
    searchString: '',
    didFirstRender: false,
    username: null,
    loading: false,
});
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
export default class ProviderView {
    static VERSION = packageJson.version;
    // Test hook (mirrors GoldenRetriever pattern): allow tests to override debounce time
    // @ts-expect-error test-only hook key
    static [Symbol.for('uppy test: searchDebounceMs')];
    plugin;
    provider;
    opts;
    isHandlingScroll = false;
    previousCheckbox = null;
    #searchDebounced;
    constructor(plugin, opts) {
        this.plugin = plugin;
        this.provider = opts.provider;
        const defaultOptions = {
            viewType: 'list',
            showTitles: true,
            showFilter: true,
            showBreadcrumbs: true,
            loadAllFiles: false,
            virtualList: false,
        };
        this.opts = { ...defaultOptions, ...opts };
        this.openFolder = this.openFolder.bind(this);
        this.logout = this.logout.bind(this);
        this.handleAuth = this.handleAuth.bind(this);
        this.handleScroll = this.handleScroll.bind(this);
        this.resetPluginState = this.resetPluginState.bind(this);
        this.donePicking = this.donePicking.bind(this);
        this.render = this.render.bind(this);
        this.cancelSelection = this.cancelSelection.bind(this);
        this.toggleCheckbox = this.toggleCheckbox.bind(this);
        this.openSearchResultFolder = this.openSearchResultFolder.bind(this);
        this.clearSearchState = this.clearSearchState.bind(this);
        // Set default state for the plugin
        this.resetPluginState();
        // todo
        // @ts-expect-error this should be typed in @uppy/dashboard.
        this.plugin.uppy.on('dashboard:close-panel', this.resetPluginState);
        this.plugin.uppy.registerRequestClient(this.provider.provider, this.provider);
        // Configure debounced search with test override
        const testHookSymbol = Symbol.for('uppy test: searchDebounceMs');
        const testWait = ProviderView[testHookSymbol];
        const wait = testWait ?? 500;
        const debounceOpts = testWait === 0 ? { leading: true, trailing: true } : undefined;
        this.#searchDebounced = debounce(this.#search, wait, debounceOpts);
    }
    resetPluginState() {
        this.plugin.setPluginState(getDefaultState(this.plugin.rootFolderId));
    }
    tearDown() {
        // Nothing.
    }
    setLoading(loading) {
        this.plugin.setPluginState({ loading });
    }
    get isLoading() {
        return this.plugin.getPluginState().loading;
    }
    cancelSelection() {
        const { partialTree } = this.plugin.getPluginState();
        const newPartialTree = partialTree.map((item) => item.type === 'root' ? item : { ...item, status: 'unchecked' });
        this.plugin.setPluginState({ partialTree: newPartialTree });
    }
    clearSearchState() {
        this.plugin.setPluginState({
            searchResults: undefined,
        });
    }
    #abortController;
    async #withAbort(op) {
        // prevent multiple requests in parallel from causing race conditions
        this.#abortController?.abort();
        const abortController = new AbortController();
        this.#abortController = abortController;
        const cancelRequest = () => {
            abortController.abort();
        };
        try {
            // @ts-expect-error this should be typed in @uppy/dashboard.
            // Even then I don't think we can make this work without adding dashboard
            // as a dependency to provider-views.
            this.plugin.uppy.on('dashboard:close-panel', cancelRequest);
            this.plugin.uppy.on('cancel-all', cancelRequest);
            await op(abortController.signal);
        }
        finally {
            // @ts-expect-error this should be typed in @uppy/dashboard.
            // Even then I don't think we can make this work without adding dashboard
            // as a dependency to provider-views.
            this.plugin.uppy.off('dashboard:close-panel', cancelRequest);
            this.plugin.uppy.off('cancel-all', cancelRequest);
            this.#abortController = undefined;
        }
    }
    async #search() {
        const { partialTree, currentFolderId, searchString } = this.plugin.getPluginState();
        const currentFolder = partialTree.find((i) => i.id === currentFolderId);
        if (searchString.trim() === '') {
            this.#abortController?.abort();
            this.clearSearchState();
            return;
        }
        this.setLoading(true);
        await this.#withAbort(async (signal) => {
            const scopePath = currentFolder.type === 'root' ? undefined : currentFolderId;
            const { items } = await this.provider.search(searchString, {
                signal,
                path: scopePath,
            });
            // For each searched file, build the entire path (from the root all the way to the leaf node)
            // This is because we need to make sure all ancestor folders are present in the partialTree before we open the folder or check the file.
            // This is needed because when the user opens a folder we need to have all its parent folders in the partialTree to be able to render the breadcrumbs correctly.
            // Similarly when the user checks a file, we need to have all it's ancestor folders in the partialTree to be able to percolateUp the checked state correctly to its ancestors.
            const { partialTree } = this.plugin.getPluginState();
            const newPartialTree = [...partialTree];
            for (const file of items) {
                // Decode URI and split into path segments
                const decodedPath = decodeURIComponent(file.requestPath);
                const segments = decodedPath.split('/').filter((s) => s.length > 0);
                // Start from root
                let parentId = this.plugin.rootFolderId;
                let isParentFolderChecked;
                // Walk through each segment starting from the root and build child nodes if they don't exist
                segments.forEach((segment, index, arr) => {
                    const pathSegments = segments.slice(0, index + 1);
                    const encodedPath = encodeURIComponent(`/${pathSegments.join('/')}`);
                    // Skip if node already exists
                    const existingNode = newPartialTree.find((n) => n.id === encodedPath && n.type !== 'root');
                    if (existingNode) {
                        parentId = encodedPath;
                        isParentFolderChecked = existingNode.status === 'checked';
                        return;
                    }
                    const isLeafNode = index === arr.length - 1;
                    let node;
                    // propagate checked state from parent to children, if the user has checked the parent folder before searching
                    // and the parent folder is an ancestor of the searched file
                    // see also afterOpenFolder which contains similar logic, we should probably refactor and reuse some
                    const status = isParentFolderChecked ? 'checked' : 'unchecked';
                    // Build the Leaf Node, it can be a file (`PartialTreeFile`) or a folder (`PartialTreeFolderNode`).
                    // Since we Already have the leaf node's data (`file`, `CompanionFile`) from the searchResults: CompanionFile[], we just use that.
                    if (isLeafNode) {
                        if (file.isFolder) {
                            node = {
                                type: 'folder',
                                id: encodedPath,
                                cached: false,
                                nextPagePath: null,
                                status,
                                parentId,
                                data: file,
                            };
                        }
                        else {
                            const restrictionError = this.validateSingleFile(file);
                            node = {
                                type: 'file',
                                id: encodedPath,
                                restrictionError,
                                status: !restrictionError ? status : 'unchecked',
                                parentId,
                                data: file,
                            };
                        }
                    }
                    else {
                        // not leaf node, so by definition it is a folder leading up to the leaf node
                        node = {
                            type: 'folder',
                            id: encodedPath,
                            cached: false,
                            nextPagePath: null,
                            status,
                            parentId,
                            data: {
                                // we don't have any data, so fill only the necessary fields
                                name: decodeURIComponent(segment),
                                icon: 'folder',
                                isFolder: true,
                            },
                        };
                    }
                    newPartialTree.push(node);
                    parentId = encodedPath; // This node becomes parent for the next iteration
                    isParentFolderChecked = status === 'checked';
                });
            }
            this.plugin.setPluginState({
                partialTree: newPartialTree,
                searchResults: items.map((item) => item.requestPath),
            });
        }).catch(handleError(this.plugin.uppy));
        this.setLoading(false);
    }
    // debounced search function is initialized in the constructor
    onSearchInput = (s) => {
        this.plugin.setPluginState({ searchString: s });
        if (this.opts.supportsSearch) {
            this.#searchDebounced();
        }
    };
    async openSearchResultFolder(folderId) {
        // stop searching
        this.plugin.setPluginState({ searchString: '' });
        // now open folder using the normal view
        await this.openFolder(folderId);
    }
    async openFolder(folderId) {
        // always switch away from the search view when opening a folder, whether it happens from the search view or by clicking breadcrumbs
        this.clearSearchState();
        this.previousCheckbox = null;
        // Returning cached folder
        const { partialTree } = this.plugin.getPluginState();
        const clickedFolder = partialTree.find((folder) => folder.id === folderId);
        if (clickedFolder.cached) {
            this.plugin.setPluginState({
                currentFolderId: folderId,
                searchString: '',
            });
            return;
        }
        this.setLoading(true);
        await this.#withAbort(async (signal) => {
            let currentPagePath = folderId;
            let currentItems = [];
            do {
                const { username, nextPagePath, items } = await this.provider.list(currentPagePath, { signal });
                // It's important to set the username during one of our first fetches
                this.plugin.setPluginState({ username });
                currentPagePath = nextPagePath;
                currentItems = currentItems.concat(items);
                this.setLoading(this.plugin.uppy.i18n('loadedXFiles', {
                    numFiles: currentItems.length,
                }));
            } while (this.opts.loadAllFiles && currentPagePath);
            const newPartialTree = PartialTreeUtils.afterOpenFolder(partialTree, currentItems, clickedFolder, currentPagePath, this.validateSingleFile);
            this.plugin.setPluginState({
                partialTree: newPartialTree,
                currentFolderId: folderId,
                searchString: '',
            });
        }).catch(handleError(this.plugin.uppy));
        this.setLoading(false);
    }
    /**
     * Removes session token on client side.
     */
    async logout() {
        await this.#withAbort(async (signal) => {
            const res = await this.provider.logout({
                signal,
            });
            // res.ok is from the JSON body, not to be confused with Response.ok
            if (res.ok) {
                if (!res.revoked) {
                    const message = this.plugin.uppy.i18n('companionUnauthorizeHint', {
                        provider: this.plugin.title,
                        url: res.manual_revoke_url,
                    });
                    this.plugin.uppy.info(message, 'info', 7000);
                }
                this.plugin.setPluginState({
                    ...getDefaultState(this.plugin.rootFolderId),
                    authenticated: false,
                });
            }
        }).catch(handleError(this.plugin.uppy));
    }
    async handleAuth(authFormData) {
        await this.#withAbort(async (signal) => {
            this.setLoading(true);
            await this.provider.login({ authFormData, signal });
            this.plugin.setPluginState({ authenticated: true });
            await Promise.all([
                this.provider.fetchPreAuthToken(),
                this.openFolder(this.plugin.rootFolderId),
            ]);
        }).catch(handleError(this.plugin.uppy));
        this.setLoading(false);
    }
    async handleScroll(event) {
        const { partialTree, currentFolderId } = this.plugin.getPluginState();
        const currentFolder = partialTree.find((i) => i.id === currentFolderId);
        if (shouldHandleScroll(event) &&
            !this.isHandlingScroll &&
            currentFolder.nextPagePath) {
            this.isHandlingScroll = true;
            await this.#withAbort(async (signal) => {
                const { nextPagePath, items } = await this.provider.list(currentFolder.nextPagePath, { signal });
                const newPartialTree = PartialTreeUtils.afterScrollFolder(partialTree, currentFolderId, items, nextPagePath, this.validateSingleFile);
                this.plugin.setPluginState({ partialTree: newPartialTree });
            }).catch(handleError(this.plugin.uppy));
            this.isHandlingScroll = false;
        }
    }
    validateSingleFile = (file) => {
        const companionFile = remoteFileObjToLocal(file);
        const result = this.plugin.uppy.validateSingleFile(companionFile);
        return result;
    };
    async donePicking() {
        const { partialTree } = this.plugin.getPluginState();
        if (this.isLoading)
            return;
        this.setLoading(true);
        await this.#withAbort(async (signal) => {
            // 1. Enrich our partialTree by fetching all 'checked' but not-yet-fetched folders
            const enrichedTree = await PartialTreeUtils.afterFill(partialTree, (path) => this.provider.list(path, { signal }), this.validateSingleFile, (n) => {
                this.setLoading(this.plugin.uppy.i18n('addedNumFiles', { numFiles: n }));
            });
            // 2. Now that we know how many files there are - recheck aggregateRestrictions!
            const aggregateRestrictionError = this.validateAggregateRestrictions(enrichedTree);
            if (aggregateRestrictionError) {
                this.plugin.setPluginState({ partialTree: enrichedTree });
                return;
            }
            // 3. Add files
            const companionFiles = getCheckedFilesWithPaths(enrichedTree);
            addFiles(companionFiles, this.plugin, this.provider);
            // 4. Reset state
            this.resetPluginState();
        }).catch(handleError(this.plugin.uppy));
        this.setLoading(false);
    }
    toggleCheckbox(ourItem, isShiftKeyPressed) {
        const { partialTree } = this.plugin.getPluginState();
        const clickedRange = getClickedRange(ourItem.id, this.getDisplayedPartialTree(), isShiftKeyPressed, this.previousCheckbox);
        const newPartialTree = PartialTreeUtils.afterToggleCheckbox(partialTree, clickedRange);
        this.plugin.setPluginState({ partialTree: newPartialTree });
        this.previousCheckbox = ourItem.id;
    }
    getDisplayedPartialTree = () => {
        const { partialTree, currentFolderId, searchString } = this.plugin.getPluginState();
        const inThisFolder = partialTree.filter((item) => item.type !== 'root' && item.parentId === currentFolderId);
        // If provider supports server side search, we don't filter the items client side
        const filtered = this.opts.supportsSearch || searchString.trim() === ''
            ? inThisFolder
            : inThisFolder.filter((item) => (item.data.name ?? this.plugin.uppy.i18n('unnamed'))
                .toLowerCase()
                .indexOf(searchString.trim().toLowerCase()) !== -1);
        return filtered;
    };
    getBreadcrumbs = () => {
        const { partialTree, currentFolderId } = this.plugin.getPluginState();
        return getBreadcrumbs(partialTree, currentFolderId);
    };
    getSelectedAmount = () => {
        const { partialTree } = this.plugin.getPluginState();
        return getNumberOfSelectedFiles(partialTree);
    };
    validateAggregateRestrictions = (partialTree) => {
        const checkedFiles = partialTree.filter((item) => item.type === 'file' && item.status === 'checked');
        const uppyFiles = checkedFiles.map((file) => file.data);
        return this.plugin.uppy.validateAggregateRestrictions(uppyFiles);
    };
    #renderSearchResults() {
        const { i18n } = this.plugin.uppy;
        const { searchResults: ids, partialTree } = this.plugin.getPluginState();
        // todo memoize this so we don't have to do it on every render
        const itemsById = new Map();
        partialTree.forEach((item) => {
            if (item.type !== 'root') {
                itemsById.set(item.id, item);
            }
        });
        // the search results view needs data from the partial tree,
        const searchResults = ids.map((id) => {
            const partialTreeItem = itemsById.get(id);
            if (partialTreeItem == null)
                throw new Error('Partial tree not complete');
            return partialTreeItem;
        });
        return (_jsx(GlobalSearchView, { searchResults: searchResults, openFolder: this.openSearchResultFolder, toggleCheckbox: this.toggleCheckbox, i18n: i18n }));
    }
    render(state, viewOptions = {}) {
        const { didFirstRender } = this.plugin.getPluginState();
        const { i18n } = this.plugin.uppy;
        if (!didFirstRender) {
            this.plugin.setPluginState({ didFirstRender: true });
            this.provider.fetchPreAuthToken();
            this.openFolder(this.plugin.rootFolderId);
        }
        const opts = { ...this.opts, ...viewOptions };
        const { authenticated, loading } = this.plugin.getPluginState();
        const pluginIcon = this.plugin.icon || defaultPickerIcon;
        if (authenticated === false) {
            return (_jsx(AuthView, { pluginName: this.plugin.title, pluginIcon: pluginIcon, handleAuth: this.handleAuth, i18n: this.plugin.uppy.i18n, renderForm: opts.renderAuthForm, loading: loading }));
        }
        const { partialTree, username, searchString, searchResults } = this.plugin.getPluginState();
        const breadcrumbs = this.getBreadcrumbs();
        return (_jsxs("div", { className: classNames('uppy-ProviderBrowser', `uppy-ProviderBrowser-viewType--${opts.viewType}`), children: [_jsx(Header, { showBreadcrumbs: opts.showBreadcrumbs, openFolder: this.openFolder, breadcrumbs: breadcrumbs, pluginIcon: pluginIcon, title: this.plugin.title, logout: this.logout, username: username, i18n: i18n }), opts.showFilter && (_jsx(FilterInput, { value: searchString, onChange: (s) => this.onSearchInput(s), onSubmit: () => { }, inputLabel: i18n('filter'), i18n: i18n })), searchResults ? (this.#renderSearchResults()) : (_jsx(Browser, { toggleCheckbox: this.toggleCheckbox, displayedPartialTree: this.getDisplayedPartialTree(), openFolder: this.openFolder, virtualList: opts.virtualList, noResultsLabel: i18n('noFilesFound'), handleScroll: this.handleScroll, viewType: opts.viewType, showTitles: opts.showTitles, i18n: this.plugin.uppy.i18n, isLoading: loading, utmSource: "Companion" })), _jsx(FooterActions, { partialTree: partialTree, donePicking: this.donePicking, cancelSelection: this.cancelSelection, i18n: i18n, validateAggregateRestrictions: this.validateAggregateRestrictions })] }));
    }
}
