import type { ComponentChild } from 'preact';
interface SearchViewProps {
    value: string;
    onChange: (value: string) => void;
    onSubmit: () => void;
    inputLabel: string;
    loading?: boolean;
    children: ComponentChild;
}
/**
 * SearchView component for search with a submit button.
 * Typically used for initial search views or forms that require explicit submission.
 * The children prop is rendered as the button content, allowing full control over button text and loading states.
 */
declare function SearchView({ value, onChange, onSubmit, inputLabel, loading, children, }: SearchViewProps): import("preact").JSX.Element;
export default SearchView;
//# sourceMappingURL=SearchView.d.ts.map