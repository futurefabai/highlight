import React, { useContext, useEffect, useMemo, useRef, useState } from 'react';
import Skeleton from 'react-loading-skeleton';
import { useHistory } from 'react-router-dom';
import { Virtuoso, VirtuosoHandle } from 'react-virtuoso';

import Input from '../../../../../components/Input/Input';
import ReplayerContext, { ReplayerState } from '../../../ReplayerContext';
import { useErrorModalContext } from '../../ErrorModalContext/ErrorModalContext';
import devStyles from '../DevToolsWindow.module.scss';
import ErrorCard, { ErrorCardState } from './components/ErrorCard/ErrorCard';
import styles from './ErrorsPage.module.scss';
import { findLastActiveEventIndex } from './utils/utils';

export interface ErrorsPageHistoryState {
    errorCardIndex: number;
}

const ErrorsPage = () => {
    const [lastActiveErrorIndex, setLastActiveErrorIndex] = useState(-1);
    const virtuoso = useRef<VirtuosoHandle>(null);
    const [isInteractingWithErrors, setIsInteractingWithErrors] = useState(
        false
    );
    const [filterSearchTerm, setFilterSearchTerm] = useState('');
    const history = useHistory<ErrorsPageHistoryState>();
    const { errors: allErrors, state, time, replayer } = useContext(
        ReplayerContext
    );
    const { setSelectedError } = useErrorModalContext();

    const loading = state === ReplayerState.Loading;

    /** Only errors recorded after this feature was released will have the timestamp. */
    const hasTimestamp =
        !loading && allErrors?.every((error) => !!error.timestamp);

    useEffect(() => {
        if (!isInteractingWithErrors && hasTimestamp && replayer) {
            const index = findLastActiveEventIndex(
                time,
                replayer.getMetaData().startTime,
                allErrors
            );
            setLastActiveErrorIndex(index);
        }
    }, [allErrors, hasTimestamp, isInteractingWithErrors, replayer, time]);

    useEffect(() => {
        if (virtuoso.current) {
            // Scrolls to the error card the user originally clicked on. This only happens if the user clicked on an error card from the player page which navigates them to the error page. From there there navigate back using the browser's back navigation.
            if (
                history.location.state?.errorCardIndex !== undefined &&
                state === ReplayerState.Playing
            ) {
                virtuoso.current.scrollToIndex(
                    history.location.state.errorCardIndex
                );
            } else {
                virtuoso.current.scrollToIndex(lastActiveErrorIndex);
            }
        }
    }, [history.location.state?.errorCardIndex, lastActiveErrorIndex, state]);

    const errorsToRender = useMemo(() => {
        if (filterSearchTerm === '') {
            return allErrors;
        }

        return allErrors.filter((error) => {
            const normalizedFilterSearchTerm = filterSearchTerm.toLocaleLowerCase();

            return error.event.some(
                (line) =>
                    line
                        ?.toLocaleLowerCase()
                        .includes(normalizedFilterSearchTerm) ||
                    error.source
                        ?.toLocaleLowerCase()
                        .includes(normalizedFilterSearchTerm)
            );
        });
    }, [allErrors, filterSearchTerm]);

    return (
        <div className={styles.errorsPageWrapper}>
            <div className={devStyles.topBar}>
                <div className={devStyles.optionsWrapper}>
                    <div className={styles.filterContainer}>
                        <Input
                            allowClear
                            placeholder="Filter"
                            value={filterSearchTerm}
                            onChange={(event) => {
                                setFilterSearchTerm(event.target.value);
                            }}
                            size="small"
                            disabled={loading}
                        />
                    </div>
                </div>
            </div>
            <div className={styles.errorList}>
                {loading ? (
                    <div className={styles.skeleton}>
                        <Skeleton
                            count={2}
                            style={{ height: 25, marginBottom: 11 }}
                        />
                    </div>
                ) : !allErrors.length ? (
                    <div className={devStyles.emptySection}>
                        There are no errors for this session.
                    </div>
                ) : (
                    <Virtuoso
                        onMouseEnter={() => {
                            setIsInteractingWithErrors(true);
                        }}
                        onMouseLeave={() => {
                            setIsInteractingWithErrors(false);
                        }}
                        ref={virtuoso}
                        overscan={500}
                        data={errorsToRender}
                        itemContent={(index, error) => (
                            <ErrorCard
                                searchQuery={filterSearchTerm}
                                key={error?.id}
                                error={error}
                                state={
                                    hasTimestamp
                                        ? index === lastActiveErrorIndex
                                            ? ErrorCardState.Active
                                            : ErrorCardState.Inactive
                                        : ErrorCardState.Unknown
                                }
                                setSelectedError={() => {
                                    setSelectedError(error);
                                }}
                            />
                        )}
                    />
                )}
            </div>
        </div>
    );
};

export default ErrorsPage;
