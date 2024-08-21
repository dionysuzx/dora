$_network = {};

// Select a node and its neighborhood edges
$_network.selectNode = function (cy, nodeId) {
  elem = cy.getElementById(nodeId)
  cy.elements().unselect();
  elem.select();
  console.log(elem.neighborhood());
  elem.neighborhood().edges().select();
}

// Isolate a node and only show it and its neighborhood
$_network.isolateNode = function (cy, nodeId) {
  elem = cy.getElementById(nodeId)
  cy.elements().hide().unselect();
  elem.show().select();
  edges = elem.neighborhood().edges();
  edges.show();
  edges.select();
  elem.neighborhood().nodes().show();
}

// Show all nodes and edges
$_network.showAll = function (cy) {
  cy.elements().show().unselect();
}

// Available Layouts
$_network.layouts = {
  fcose : function(nodeCount){
    return {
      name: 'fcose',
      idealEdgeLength: 5 * nodeCount,
      nestingFactor: 1.2,
      animate: false,
      stop: function() {
        $("#nodemap-loading").hide();
      }
    }
  },
  circle : function() {
    return {
      name: 'circle',
      animate: false,
    }
  },
  grid : function() {
    return {
      name: 'grid',
      animate: false,
    }
  },
}

// Default layout
$_network.defaultLayout = $_network.layouts.fcose;


// Default stylesheet
$_network.defaultStylesheet = cytoscape.stylesheet()
  .selector('node')
    .css({
      'height': 20,
      'width': 20,
      'background-fit': 'cover',
      'border-color': '#0077B6',
      'border-width': 1,
      'border-opacity': 1,
    })
  .selector('edge')
    .css({
      'curve-style': 'bezier',
      'width': 0.5,
      'target-arrow-shape': 'vee',
      'line-color': '#0077B6',
      'target-arrow-color': '#0077B6',
      'arrow-scale': 0.5,
    })
  .selector('node[label]')
    .css({
      'label': 'data(label)',
    })
  .selector('.bottom-center')
    .css({
      "text-valign": "bottom",
      "text-halign": "center",
      "color": "#ffffff",
      "font-size": 4,
    })
  .selector('node:selected, edge:selected')
    .css({
      'border-color': '#FFA500',
      'background-color': '#FFA500',
      'line-color': '#FFA500',
      'target-arrow-color': '#FFA500',
      'source-arrow-color': '#FFA500',
      'opacity': 1
});


$_network.fitAnimated = function (cy, layout) {
  cy.animate({
    fit: { eles: cy.$() },
    duration: 500,
    complete: function () {
      setTimeout(function () {
        layout.animate = true;
        layout.animationDuration = 2000;
        layout.fit = true;
        layout.directed = true;
        cy.layout(layout).run();
      }, 500);
    },
  });
}


// Create a cytoscape network
$_network.create = function (container, data){
  var stylesheet = $_network.defaultStylesheet;
  var cytoElements = [];
  for (var i = 0; i < data.nodes.length; i++) {
    // Create nodes
    data.nodes[i].title = data.nodes[i].id;
    if (data.nodes[i].id != "") {
      cytoElements.push(
        {
          data: data.nodes[i],
          classes: "bottom-center",
        }
      );
      // Add style to nodes
      stylesheet.selector('#' + data.nodes[i].id).css({
          'background-image': '/identicon?key=' + data.nodes[i].id
      });
    }
  }
  for (var i = 0; i < data.edges.length; i++) {
    // Create edges
    cytoElements.push({
      data: {
        id: data.edges[i].from + "-" + data.edges[i].to,
        source: data.edges[i].from,
        target: data.edges[i].to
      }
    });
  }

  var cy = window.cy = cytoscape({
      container: container,
      style: stylesheet,
      layout: $_network.defaultLayout(data.nodes.length),
      elements: cytoElements,
      wheelSensitivity: 0.1,
    });

  cy.on('tap', 'node', function(evt){
    evt.preventDefault();
    console.log(evt.target.id());
    $_network.isolateNode(cy, evt.target.id());
    $(".collapse.peerInfo").collapse("hide");
    $("#peerInfo-" + evt.target.id()).collapse("show");
  });

  cy.on('tap', function(event){
    var evtTarget = event.target;
    if( evtTarget === cy ){ // tap on background
      $_network.showAll(cy);
      window.location.hash = "";
      $(".collapse.peerInfo").collapse("hide");
    }
  });

  return cy;
}
