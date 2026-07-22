document.addEventListener("DOMContentLoaded", function() {
    // Initialize Masonry for standard pin grids
    const grids = document.querySelectorAll('.grid');
    grids.forEach(grid => {
        new Masonry(grid, {
            itemSelector: '.grid-item',
            columnWidth: 240,
            gutter: 16,
            fitWidth: true, // Centers the grid in its parent container
            transitionDuration: 0 // Avoid weird animations on load
        });
    });

    // Initialize Masonry for boards and users
    const boardGrids = document.querySelectorAll('.boards-grid, .users-grid');
    boardGrids.forEach(grid => {
        new Masonry(grid, {
            itemSelector: '.masonry-item',
            columnWidth: 280,
            gutter: 24,
            fitWidth: true,
            transitionDuration: 0
        });
    });
    
    window.recalculateMasonry = function() {
        grids.forEach(grid => Masonry.data(grid)?.layout());
        boardGrids.forEach(grid => Masonry.data(grid)?.layout());
    };

    // Hide 'more' button if description is not overflowing
    function updateShowMoreLabels() {
        document.querySelectorAll('.post-description').forEach(function(desc) {
            var label = desc.nextElementSibling;
            if (label && label.classList.contains('show-more-label')) {
                // If scrollHeight is strictly greater than clientHeight, it's clamped
                if (desc.scrollHeight > desc.clientHeight) {
                    label.style.display = 'inline-block';
                } else {
                    label.style.display = 'none';
                }
            }
        });
    }

    // Run this whenever we recalculate
    const originalRecalculate = window.recalculateMasonry;
    window.recalculateMasonry = function() {
        updateShowMoreLabels();
        originalRecalculate();
    };

    // Listen for description toggles
    document.addEventListener('change', function(e) {
        if (e.target.classList.contains('desc-toggle')) {
            // Need a tiny delay for CSS max-height unset to take effect
            setTimeout(window.recalculateMasonry, 10);
            setTimeout(window.recalculateMasonry, 150); // safety catch for transitions
        }
    });

    // Initial run for labels
    updateShowMoreLabels();
});
